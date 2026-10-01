package runs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
)

func TestRegistryCancelBindsCause(t *testing.T) {
	reg := NewRegistry()
	run, err := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_1", JobID: "job_1", Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_1"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate ID = %v, want ErrBusy", err)
	}
	if reg.Count(models.RunBackup) != 1 || reg.Count(models.RunRestore) != 0 {
		t.Fatal("counts are wrong")
	}

	// A cancellation before Bind takes effect when the run binds its context.
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if err := reg.Cancel("bkp_1", Cancellation{By: "alice", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Cancel("bkp_1", Cancellation{By: "bob"}); err != nil {
		t.Fatalf("a second cancellation = %v", err)
	}
	ctx := run.Bind(context.Background())
	if FromContext(ctx) != run {
		t.Fatal("the bound context does not carry the run")
	}
	<-ctx.Done()
	c := CancellationOf(ctx)
	if c == nil || c.By != "alice" || !c.At.Equal(at) || !errors.Is(c, ErrCancelled) || c.Error() != "cancelled by alice" {
		t.Fatalf("cancellation = %+v", c)
	}
	if p := run.Snapshot(); !p.Cancelling || p.Phase != models.PhaseCancelling || p.JobID != "job_1" {
		t.Fatalf("snapshot = %+v", p)
	}

	run.End()
	run.End()
	if err := reg.Cancel("bkp_1", Cancellation{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel after End = %v", err)
	}
	if err := reg.Cancel("unknown", Cancellation{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel of an unknown run = %v", err)
	}
	if len(reg.Snapshots()) != 0 || reg.Progress("bkp_1") != nil {
		t.Fatal("an ended run is still listed")
	}
}

func TestCancellationOfIgnoresOtherCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	if CancellationOf(ctx) != nil {
		t.Fatal("a live context has no cancellation")
	}
	cancel(errors.New("timeout"))
	if CancellationOf(ctx) != nil {
		t.Fatal("another cause is not a cancellation")
	}
	reason := &Cancellation{By: SystemActor, Reason: "cancelled: application force quit"}
	if reason.Error() != "cancelled: application force quit" {
		t.Fatalf("reason error = %q", reason.Error())
	}
	if (&Cancellation{Reason: "quit"}).Error() != "cancelled: quit" || (&Cancellation{}).Error() != "cancelled" {
		t.Fatal("cancellation messages are wrong")
	}
}

func TestNilRegistryAndRun(t *testing.T) {
	var reg *Registry
	run, err := reg.Register(Meta{ID: "x"})
	if run != nil || err != nil {
		t.Fatalf("nil registry = %v, %v", run, err)
	}
	ctx := context.Background()
	if run.Bind(ctx) != ctx {
		t.Fatal("a nil run must keep the context")
	}
	run.Printf("ignored")
	run.Phase(models.PhaseDumping, models.RunPhases{})
	run.StartTransfer(10)
	run.AddBytes(5)
	run.End()
	if _, err := run.ToolOutput().Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if r := run.CountingReader(strings.NewReader("abc")); r == nil {
		t.Fatal("nil counting reader")
	}
	if reg.Count(models.RunBackup) != 0 || len(reg.Snapshots()) != 0 || len(reg.CancelAll(Cancellation{})) != 0 {
		t.Fatal("a nil registry tracks nothing")
	}
	if _, err := reg.OpenLog("x"); !errors.Is(err, runlog.ErrNotFound) {
		t.Fatalf("OpenLog on a nil registry = %v", err)
	}
	if FromContext(ctx) != nil || CancellationOf(ctx) != nil {
		t.Fatal("a plain context carries no run")
	}
}

func TestRunProgressFromToolOutput(t *testing.T) {
	logs := runlog.NewDir(t.TempDir())
	reg := NewRegistry(WithLogs(logs))
	run, err := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_p", Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := run.Bind(context.Background())
	run.Phase(models.PhaseDumping, models.RunPhases{Started: models.Stamp(time.Now())})
	out := run.ToolOutput()
	fmt.Fprint(out, "2025-06-02T09:14:01.104+0000\twriting shop.orders to archive on stdout\n")
	fmt.Fprint(out, "2025-06-02T09:14:01.105+0000\twriting shop.users to archive on stdout\n")
	fmt.Fprint(out, "2025-06-02T09:14:03.105+0000\t[####....]  shop.orders  1000/4000  (25.0%)\n")
	fmt.Fprint(out, "2025-06-02T09:14:03.200+0000\tdone dumping shop.users (")
	fmt.Fprint(out, "1000 documents)\nunfinished line")
	_ = out.Close()
	n, _ := io.Copy(io.Discard, run.CountingReader(strings.NewReader(strings.Repeat("x", 4096))))

	p := run.Snapshot()
	if p.ID != "bkp_p" || p.Kind != models.RunBackup || p.Database != "shop" || p.Phase != models.PhaseDumping {
		t.Fatalf("snapshot = %+v", p)
	}
	if p.Bytes != n || p.Documents != 2000 || p.CollectionsTotal != 2 || p.CollectionsDone != 1 ||
		p.CurrentCollection != "shop.orders" || p.Phases.Started == nil || p.Phases.Queued == nil {
		t.Fatalf("snapshot = %+v", p)
	}
	if p.Percent == nil || *p.Percent != 40 {
		t.Fatalf("percent = %v, want 40 (2000 of 5000 documents)", p.Percent)
	}

	// A restore's percent comes from the bytes read of the artifact.
	run.StartTransfer(8192)
	run.AddBytes(2048)
	if p := run.Snapshot(); p.Percent == nil || *p.Percent != 25 || p.TotalBytes != 8192 {
		t.Fatalf("byte percent = %+v", p)
	}

	r, err := reg.OpenLog("bkp_p")
	if err != nil {
		t.Fatal(err)
	}
	tail, _ := r.Tail(2)
	_ = r.Close()
	if !strings.Contains(string(tail), "done dumping shop.users (1000 documents)") || !strings.Contains(string(tail), "unfinished line") {
		t.Fatalf("live log tail = %q", tail)
	}
	if err := reg.RemoveLog("bkp_p"); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Open("bkp_p"); err != nil {
		t.Fatal("the log of an active run must not be removed")
	}
	_ = ctx
	run.End()
	if r, err := reg.OpenLog("bkp_p"); err != nil {
		t.Fatalf("finished log: %v", err)
	} else {
		_ = r.Close()
	}
	if err := reg.RemoveLog("bkp_p"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.OpenLog("bkp_p"); !errors.Is(err, runlog.ErrNotFound) {
		t.Fatalf("removed log = %v", err)
	}
}

func TestCancelAll(t *testing.T) {
	reg := NewRegistry()
	var ctxs []context.Context
	for i := range 3 {
		run, _ := reg.Register(Meta{Kind: models.RunRestore, ID: fmt.Sprintf("rst_%d", i)})
		ctxs = append(ctxs, run.Bind(context.Background()))
		defer run.End()
	}
	if ids := reg.CancelAll(Cancellation{By: SystemActor, Reason: "cancelled: application force quit"}); len(ids) != 3 || ids[0] != "rst_0" {
		t.Fatalf("cancelled %v", ids)
	}
	// Runs already cancelled are not cancelled again.
	if ids := reg.CancelAll(Cancellation{By: SystemActor}); len(ids) != 0 {
		t.Fatalf("cancelled again: %v", ids)
	}
	for _, ctx := range ctxs {
		if c := CancellationOf(ctx); c == nil || c.By != SystemActor {
			t.Fatalf("cancellation = %+v", c)
		}
	}
}

func TestFinishingRunsCannotBeCancelled(t *testing.T) {
	reg := NewRegistry()
	run, _ := reg.Register(Meta{Kind: models.RunRestore, ID: "rst_f"})
	ctx := run.Bind(context.Background())
	defer run.End()
	run.Finishing()
	if p := run.Snapshot(); p.Phase != models.PhaseFinishing || p.Cancelling {
		t.Fatalf("snapshot = %+v", p)
	}
	if err := reg.Cancel("rst_f", Cancellation{By: "alice"}); !errors.Is(err, ErrFinishing) {
		t.Fatalf("cancel while finishing = %v, want ErrFinishing", err)
	}
	if ids := reg.CancelAll(Cancellation{By: SystemActor}); len(ids) != 0 {
		t.Fatalf("CancelAll cancelled a finishing run: %v", ids)
	}
	if ctx.Err() != nil || run.Cancellation() != nil {
		t.Fatal("a finishing run must keep running")
	}
}

// TestCancellationBeforeBindSurvivesAnEndedParent covers a force quit: the runs are
// cancelled, then the application shuts down, and a run that binds its context only
// afterwards still reports the force quit as its cancellation.
func TestCancellationBeforeBindSurvivesAnEndedParent(t *testing.T) {
	reg := NewRegistry()
	run, _ := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_late_bind"})
	defer run.End()
	if ids := reg.CancelAll(Cancellation{By: SystemActor, Reason: "cancelled: application force quit"}); len(ids) != 1 {
		t.Fatalf("cancelled %v", ids)
	}
	parent, shutdown := context.WithCancel(context.Background())
	shutdown()
	ctx := run.Bind(parent)
	c := CancellationOf(ctx)
	if c == nil || c.Reason != "cancelled: application force quit" || c.By != SystemActor {
		t.Fatalf("cancellation = %+v; want the force quit", c)
	}
}

// TestCancelAfterTheRunStoppedIsRefused keeps a timeout or shutdown from being
// reported as a cancellation requested afterwards.
func TestCancelAfterTheRunStoppedIsRefused(t *testing.T) {
	reg := NewRegistry()
	run, _ := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_stopped"})
	defer run.End()
	parent, shutdown := context.WithCancel(context.Background())
	ctx := run.Bind(parent)
	shutdown()
	if err := reg.Cancel("bkp_stopped", Cancellation{By: "alice"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel after the run stopped = %v, want ErrNotRunning", err)
	}
	if c := CancellationOf(ctx); c != nil {
		t.Fatalf("a shut-down run reports cancellation %+v", c)
	}
}
