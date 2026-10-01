package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// cancellingRunner is a fake mongorestore: it prints 100.12-style progress lines,
// consumes its input, has the registry cancel the run and then behaves like a killed
// process (stderr closes and Wait fails once the context is done).
func cancellingRunner(reg *runs.Registry, id string, called *bool) ProcessRunner {
	return func(ctx context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		*called = true
		pr, pw := io.Pipe()
		go func() {
			fmt.Fprintln(pw, "2026-10-01T10:00:00.000+0000\tpreparing collections to restore from")
			fmt.Fprintln(pw, "2026-10-01T10:00:00.010+0000\treading metadata for db_rescue.orders from archive on stdin")
			fmt.Fprintln(pw, "2026-10-01T10:00:00.020+0000\trestoring db_rescue.orders from archive on stdin")
			fmt.Fprintln(pw, "2026-10-01T10:00:01.000+0000\t[######..................]  db_rescue.orders  1.20MB/4.80MB  (25.0%)")
			_, _ = io.Copy(io.Discard, stdin)
			_ = reg.Cancel(id, runs.Cancellation{By: "alice"})
			<-ctx.Done()
			_ = pw.Close()
		}()
		return pr, func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
}

func TestCancelledCloneRestoreDropsTheClone(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	logDir := t.TempDir()
	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(logDir)))
	admin := &fakeAdmin{}
	req := models.RestoreRequest{BackupID: src.ID}
	engine := NewEngine(store, "mongodb://localhost:27017", WithDatabaseAdmin(admin))
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	engine.runner = cancellingRunner(reg, record.ID, &called)
	run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: record.ID, Database: record.TargetDatabase})

	got, err := engine.Execute(run.Bind(context.Background()), req, src, record)
	run.End()
	if !errors.Is(err, runs.ErrCancelled) {
		t.Fatalf("expected a cancellation, got %v", err)
	}
	if got.Status != models.RestoreStatusCancelled || got.CancelledBy != "alice" || got.CancelledAt == nil {
		t.Fatalf("record = %s by %q", got.Status, got.CancelledBy)
	}
	if len(admin.dropped) != 1 || admin.dropped[0] != record.TargetDatabase {
		t.Fatalf("dropped %v; want the partial clone %s", admin.dropped, record.TargetDatabase)
	}
	if !strings.Contains(got.ErrorMessage, "restore cancelled by alice") || !strings.Contains(got.ErrorMessage, "was dropped") {
		t.Fatalf("message = %q", got.ErrorMessage)
	}
	if got.InPlace || got.Warning != "" || got.Phases.Finished == nil || got.Phases.RestoreDone != nil {
		t.Fatalf("in place %v, warning %q, phases %+v", got.InPlace, got.Warning, got.Phases)
	}
	raw, err := os.ReadFile(filepath.Join(logDir, record.ID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"restoring db_rescue.orders from archive on stdin", "1.20MB/4.80MB", "cancellation requested (cancelled by alice)", "was dropped"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("run log misses %q:\n%s", want, raw)
		}
	}
}

func TestCancelledInPlaceRestoreWarnsLoudly(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	reg := runs.NewRegistry()
	admin := &fakeAdmin{}
	safe := false
	req := models.RestoreRequest{BackupID: src.ID, SafeClone: &safe, ConfirmInPlace: true}
	engine := NewEngine(store, "mongodb://localhost:27017", WithDatabaseAdmin(admin))
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	engine.runner = cancellingRunner(reg, record.ID, &called)
	run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: record.ID})

	got, err := engine.Execute(run.Bind(context.Background()), req, src, record)
	run.End()
	if !errors.Is(err, runs.ErrCancelled) || got.Status != models.RestoreStatusCancelled {
		t.Fatalf("got %s, %v", got.Status, err)
	}
	if len(admin.dropped) != 0 {
		t.Fatalf("an in-place target must never be dropped, dropped %v", admin.dropped)
	}
	if !got.InPlace || !strings.Contains(got.Warning, "PARTIALLY RESTORED") || !strings.Contains(got.ErrorMessage, "WARNING") ||
		!strings.Contains(got.ErrorMessage, "db") {
		t.Fatalf("in place %v, warning %q, message %q", got.InPlace, got.Warning, got.ErrorMessage)
	}
	if !got.Verified || got.Phases.VerifyDone == nil {
		t.Fatalf("in-place restores are verified first: %+v", got.Phases)
	}
}

func TestRestoreCancelledBeforeMongorestoreLeavesTargetUntouched(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	reg := runs.NewRegistry()
	admin := &fakeAdmin{}
	req := models.RestoreRequest{BackupID: src.ID}
	engine := NewEngine(store, "mongodb://localhost:27017", WithDatabaseAdmin(admin))
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	engine.runner = cancellingRunner(reg, record.ID, &called)
	run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: record.ID})
	if err = reg.Cancel(record.ID, runs.Cancellation{By: "API key ci"}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Execute(run.Bind(context.Background()), req, src, record)
	run.End()
	if !errors.Is(err, runs.ErrCancelled) || got.Status != models.RestoreStatusCancelled || got.CancelledBy != "API key ci" {
		t.Fatalf("got %s by %q, %v", got.Status, got.CancelledBy, err)
	}
	if called || len(admin.dropped) != 0 || !strings.Contains(got.ErrorMessage, "the target is untouched") {
		t.Fatalf("mongorestore started %v, dropped %v, message %q", called, admin.dropped, got.ErrorMessage)
	}
}
