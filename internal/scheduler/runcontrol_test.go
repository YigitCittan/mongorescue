package scheduler

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestScheduledRunIsTrackedAndCancellable checks that a cron-triggered run is stored
// as in progress while it runs, is tracked by the run registry, and ends as cancelled
// when it is cancelled there.
func TestScheduledRunIsTrackedAndCancellable(t *testing.T) {
	metaStore := storetest.New(t)
	blocking := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, eofReader{}, func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
	mock := storage.NewMockStorage()
	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	sched := NewScheduler(metaStore, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(blocking)), mock, nil,
		WithRunRegistry(reg))
	job := &models.Job{ID: "job_live", Database: "shop", CronExpression: "@daily", Enabled: true}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	type result struct {
		rec *models.BackupRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, err := sched.runBackupForJob(context.Background(), job)
		done <- result{rec, err}
	}()

	var running *models.BackupRecord
	for deadline := time.Now().Add(5 * time.Second); running == nil; time.Sleep(5 * time.Millisecond) {
		list, err := metaStore.ListBackupRecords(context.Background(), "shop")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].Status == models.StatusInProgress && reg.Get(list[0].ID) != nil {
			running = list[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduled run was never visible as in progress and tracked")
		}
	}
	if running.Trigger != models.TriggerScheduled || running.JobID != job.ID {
		t.Fatalf("running record = %+v", running)
	}
	if p := reg.Progress(running.ID); p == nil || p.JobID != job.ID {
		t.Fatalf("progress = %+v", p)
	}
	if err := reg.Cancel(running.ID, runs.Cancellation{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled scheduled run did not return")
	}
	if !errors.Is(res.err, runs.ErrCancelled) {
		t.Fatalf("run error = %v", res.err)
	}
	stored, err := metaStore.GetBackupRecord(context.Background(), running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.StatusCancelled || stored.CancelledBy != "alice" {
		t.Fatalf("stored = %+v", stored)
	}
	if reg.Get(running.ID) != nil {
		t.Fatal("the run is still tracked")
	}
	if updated, _ := metaStore.GetJob(context.Background(), job.ID); updated == nil || updated.LastRun == nil {
		t.Fatal("the job's last run was not recorded")
	}
}

// eofReader is an empty stderr.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestPausedJobsResumeWhenDue(t *testing.T) {
	sched, metaStore := newTestScheduler(t)
	ctx := context.Background()
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	for _, j := range []*models.Job{
		{ID: "job_due", Database: "a", CronExpression: "@daily", PausedUntil: &past},
		{ID: "job_later", Database: "b", CronExpression: "@daily", PausedUntil: &future},
		{ID: "job_paused", Database: "c", CronExpression: "@daily"},
		{ID: "job_on", Database: "d", CronExpression: "@daily", Enabled: true},
	} {
		if err := metaStore.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	sched.resumeDueJobs()

	for id, want := range map[string]bool{"job_due": true, "job_later": false, "job_paused": false, "job_on": true} {
		j, err := metaStore.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Enabled != want {
			t.Errorf("%s enabled = %v, want %v", id, j.Enabled, want)
		}
		if id == "job_due" && (j.PausedUntil != nil || j.NextRun == nil) {
			t.Errorf("resumed job = %+v; want paused_until cleared and the next run scheduled", j)
		}
		if id == "job_later" && j.PausedUntil == nil {
			t.Error("a job paused until later keeps its time")
		}
	}
	if sched.ActiveJobCount() != 1 {
		t.Fatalf("registered jobs = %d; want the resumed job", sched.ActiveJobCount())
	}
	sched.Stop()
	sched.resumeDueJobs() // a no-op after Stop
}
