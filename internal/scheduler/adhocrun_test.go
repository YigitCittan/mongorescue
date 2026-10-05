package scheduler

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// adHocOptions are the shared options of an ad-hoc run in these tests.
func adHocOptions() models.BackupOptions {
	return models.BackupOptions{ConnectionID: "conn", MongoURI: "mongodb://localhost:27017", Trigger: models.TriggerManual, Gzip: true}
}

// countingLocks returns n lock releases that count how often they are called.
func countingLocks(n int, released *atomic.Int32) []func() {
	locks := make([]func(), n)
	for i := range locks {
		locks[i] = func() { released.Add(1) }
	}
	return locks
}

func TestAdHocRunBacksUpEachDatabaseUnderOneRun(t *testing.T) {
	f := newMultiFixture(t)
	f.failing["b"] = true
	var released atomic.Int32
	plan, err := f.sched.PrepareAdHocRun(AdHocRun{
		Options: adHocOptions(), Databases: []string{"a", "b", "c"}, Locks: countingLocks(3, &released),
		Busy: []string{"d"}, Parallelism: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Multi() || !plan.AdHoc() || plan.Job != nil || len(plan.Records) != 3 {
		t.Fatalf("plan = %+v; want an ad-hoc run of 3 databases", plan)
	}
	ctx := context.Background()
	if err = f.sched.BeginJobRun(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, rec := range plan.Records {
		if f.reg.Get(rec.ID) == nil {
			t.Errorf("queued backup of %s is not tracked", rec.Database)
		}
	}
	run, err := f.sched.ExecuteJobRun(ctx, plan)
	if !errors.Is(err, ErrRunFailed) {
		t.Errorf("run error = %v; want ErrRunFailed (b failed, d was busy)", err)
	}
	if run.Status != models.JobRunPartial || run.JobID != "" || len(run.Databases) != 4 {
		t.Fatalf("run = %+v; want partial over 4 databases", run)
	}
	if d := run.Databases[3]; d.Database != "d" || d.BackupID != "" || d.Status != models.StatusFailed || d.Error != BusyError("d") {
		t.Errorf("busy database = %+v", d)
	}
	if n := released.Load(); n != 3 {
		t.Errorf("%d locks released; want each of the 3 taken", n)
	}
	want := map[string]models.BackupStatus{"a": models.StatusCompleted, "b": models.StatusFailed, "c": models.StatusCompleted}
	for _, rec := range plan.Records {
		stored, getErr := f.store.GetBackupRecord(ctx, rec.ID)
		if getErr != nil || stored.RunID != run.ID || stored.JobID != "" || stored.Status != want[stored.Database] || stored.Trigger != models.TriggerManual {
			t.Errorf("backup of %s = %+v, %v", rec.Database, stored, getErr)
		}
	}
	slices.Sort(f.dumped)
	if !slices.Equal(f.dumped, []string{"a", "b", "c"}) {
		t.Errorf("dumped %v; want each started database once", f.dumped)
	}
	// The run belongs to no job: nothing is stored as a job run, and one summary is
	// published for notifications.
	if list, _ := f.store.ListJobRuns(ctx, "", 10); len(list) != 0 {
		t.Errorf("stored job runs = %+v; want none", list)
	}
	sums := summaries(f.events())
	if len(sums) != 1 || sums[0].Run == nil || sums[0].RunID != run.ID || sums[0].Type != events.BackupFailed {
		t.Errorf("summary events = %+v; want one backup.failed for the run", sums)
	}
}

func TestAdHocRunOfOneDatabaseKeepsItsCollections(t *testing.T) {
	f := newMultiFixture(t)
	opts := adHocOptions()
	opts.Collections = []string{"orders"}
	plan, err := f.sched.PrepareAdHocRun(AdHocRun{Options: opts, Databases: []string{"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := plan.First(); rec.Database != "shop" || !slices.Equal(rec.Collections, []string{"orders"}) || rec.RunID != plan.Run.ID {
		t.Errorf("record = %+v; want shop.orders in the run", rec)
	}
	// Without locks the run takes each database's lock when its turn comes.
	if err = f.sched.BeginJobRun(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if run, runErr := f.sched.ExecuteJobRun(context.Background(), plan); runErr != nil || run.Status != models.JobRunOK {
		t.Errorf("run = %+v, %v; want ok", run, runErr)
	}
}

func TestPrepareAdHocRunRefusals(t *testing.T) {
	f := newMultiFixture(t)
	for name, req := range map[string]AdHocRun{
		"no databases":  {Options: adHocOptions()},
		"lock mismatch": {Options: adHocOptions(), Databases: []string{"a", "b"}, Locks: []func(){func() {}}},
	} {
		if _, err := f.sched.PrepareAdHocRun(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := f.sched.PrepareAdHocRun(AdHocRun{Options: adHocOptions()}); !errors.Is(err, ErrInvalidAdHocRun) {
		t.Errorf("no databases: %v; want ErrInvalidAdHocRun", err)
	}
}

func TestAbandonedAdHocRunReleasesItsLocks(t *testing.T) {
	f := newMultiFixture(t)
	var released atomic.Int32
	plan, err := f.sched.PrepareAdHocRun(AdHocRun{Options: adHocOptions(), Databases: []string{"a", "b"}, Locks: countingLocks(2, &released)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.sched.BeginJobRun(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	f.sched.AbandonJobRun(context.Background(), plan, runs.ErrShuttingDown)
	if n := released.Load(); n != 2 {
		t.Errorf("%d locks released; want 2", n)
	}
	for _, rec := range plan.Records {
		if stored, _ := f.store.GetBackupRecord(context.Background(), rec.ID); stored == nil || stored.Status != models.StatusFailed {
			t.Errorf("abandoned backup = %+v; want failed", stored)
		}
	}
	for _, rec := range plan.Records {
		if f.reg.Get(rec.ID) != nil {
			t.Errorf("abandoned backup of %s is still tracked", rec.Database)
		}
	}
}

func TestCancellingAnAdHocRunStopsAllItsDatabases(t *testing.T) {
	metaStore := storetest.New(t)
	var started atomic.Int32
	blocking := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		started.Add(1)
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, eofReader{}, func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
	mock := storage.NewMockStorage()
	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	pub := &recordingPublisher{}
	sched := NewScheduler(metaStore, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(blocking)), mock, nil,
		WithRunRegistry(reg), WithPublisher(pub))
	var released atomic.Int32
	plan, err := sched.PrepareAdHocRun(AdHocRun{Options: adHocOptions(), Databases: []string{"a", "b", "c"}, Locks: countingLocks(3, &released)})
	if err != nil {
		t.Fatal(err)
	}
	if err = sched.BeginJobRun(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	done := make(chan *models.JobRun, 1)
	go func() {
		run, _ := sched.ExecuteJobRun(context.Background(), plan)
		done <- run
	}()
	for deadline := time.Now().Add(5 * time.Second); started.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first database never started")
		}
	}
	// Cancelling only the running database reaches the whole run (v0.15.1).
	if err = reg.Get(plan.Records[0].ID).Cancel(runs.Cancellation{By: "alice", Kind: runs.ActorUser}); err != nil {
		t.Fatal(err)
	}
	var run *models.JobRun
	select {
	case run = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop")
	}
	if n := started.Load(); n != 1 {
		t.Errorf("mongodump started %d times; the waiting databases must not start after the cancel", n)
	}
	if run.Status != models.JobRunCancelled || released.Load() != 3 {
		t.Errorf("run status = %s, %d locks released; want cancelled and 3", run.Status, released.Load())
	}
	for _, rec := range plan.Records {
		stored, getErr := metaStore.GetBackupRecord(context.Background(), rec.ID)
		if getErr != nil || stored.Status != models.StatusCancelled {
			t.Errorf("backup of %s = %+v, %v; want cancelled", rec.Database, stored, getErr)
		}
	}
	if sums := summaries(pub.got); len(sums) != 1 || sums[0].Type != events.BackupCancelled {
		t.Errorf("summary events = %+v; want one backup.cancelled", sums)
	}
}
