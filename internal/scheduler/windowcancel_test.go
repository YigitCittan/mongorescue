package scheduler

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// releasableRunner is a mongodump that runs until its context ends (cancelled) or
// release is closed (completes).
func releasableRunner(started chan<- struct{}, release <-chan struct{}) backup.ProcessRunner {
	return func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				done <- ctx.Err()
			case <-release:
				_, _ = pw.Write([]byte("archive"))
				_ = pw.Close()
				done <- nil
			}
		}()
		started <- struct{}{}
		return pr, strings.NewReader(""), func() error { return <-done }, nil
	}
}

// cancel_at_window_end cancels the scheduled run it started, by its run ID; a
// manual run of the same job going at the same time keeps running.
func TestWindowEndCancelsOnlyTheScheduledRun(t *testing.T) {
	metaStore := storetest.New(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(releasableRunner(started, release)))
	reg := runs.NewRegistry()
	// 500ms before the window closes.
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithRunRegistry(reg),
		WithClock(fixedClock("2026-10-05T10:59:59.5Z")))
	job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "@hourly", Enabled: true,
		BackupWindow: window(t, "10:00", "11:00", true)}
	ctx := context.Background()
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// A manual run of the job, tracked like the operations layer tracks it.
	plan, err := s.PrepareJobRun(ctx, job.ID, models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	manual := plan.First()
	if err = metaStore.SaveBackupRecord(ctx, manual); err != nil {
		t.Fatal(err)
	}
	tracked, err := reg.Register(runs.Meta{Kind: models.RunBackup, ID: manual.ID, JobID: job.ID, Database: job.Database, Group: plan.Run.ID})
	if err != nil {
		t.Fatal(err)
	}
	manualDone := make(chan error, 1)
	go func() {
		defer tracked.End()
		_, runErr := s.ExecuteJobRun(tracked.Bind(ctx), plan)
		manualDone <- runErr
	}()
	<-started

	scheduledDone := make(chan struct{})
	go func() {
		defer close(scheduledDone)
		s.executeJob(ctx, job.ID)
	}()
	<-started
	select {
	case <-scheduledDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduled run was not cancelled when its window closed")
	}
	select {
	case runErr := <-manualDone:
		t.Fatalf("the manual run ended with the window: %v", runErr)
	default:
	}
	close(release)
	if err = <-manualDone; err != nil {
		t.Fatalf("manual run: %v", err)
	}

	recs, err := metaStore.ListBackupRecords(ctx, job.Database)
	if err != nil {
		t.Fatal(err)
	}
	var sawScheduled, sawManual bool
	for _, r := range recs {
		switch r.Trigger {
		case models.TriggerScheduled:
			sawScheduled = true
			if r.Status != models.StatusCancelled || !strings.Contains(r.ErrorMessage, WindowEndReason) {
				t.Fatalf("scheduled backup %s: %q", r.Status, r.ErrorMessage)
			}
		case models.TriggerOnDemand:
			sawManual = true
			if r.Status != models.StatusCompleted {
				t.Fatalf("manual backup %s: %q", r.Status, r.ErrorMessage)
			}
		}
	}
	if !sawScheduled || !sawManual {
		t.Fatalf("records %d (scheduled %v, manual %v)", len(recs), sawScheduled, sawManual)
	}
}
