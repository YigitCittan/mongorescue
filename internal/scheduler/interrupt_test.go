package scheduler

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// finishedRuns records the runs a RunObserver is told about.
type finishedRuns struct {
	mu          sync.Mutex
	runs        []models.JobRun
	interrupted []bool
}

func (f *finishedRuns) JobRunStarted(*models.Job, *models.JobRun) {}

func (f *finishedRuns) JobRunFinished(_ *models.Job, run *models.JobRun, interrupted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, *run)
	f.interrupted = append(f.interrupted, interrupted)
}

// A scheduled backup waiting for a slot of its connection when the runs manager
// shuts down ends as cancelled by the system: no backup.failed event, and a run
// status the heartbeat sends nothing for (heartbeat.SignalOf ignores cancelled
// runs).
func TestShutdownWhileWaitingForASlotIsNotAFailure(t *testing.T) {
	metaStore := storetest.New(t)
	manager := runs.NewManager(nil)
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		t.Error("mongodump started although the backup waited for a slot")
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "", backup.WithRunner(runner), backup.WithConnectionSlots(manager))
	pub := &recordingPublisher{}
	observer := &finishedRuns{}
	conns := resolver{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://db1:27017", MaxConcurrentBackups: 1}}
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithPublisher(pub), WithRunObserver(observer),
		WithConnectionResolver(conns), WithRunRegistry(runs.NewRegistry()))
	job := &models.Job{ID: "job_shop", Database: "shop", ConnectionID: "conn_a", CronExpression: "@daily", Enabled: true}
	ctx := context.Background()
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Another backup of the connection holds its only slot.
	held, err := manager.AcquireSlot(ctx, runs.ConnectionKey("conn_a"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.executeJob(ctx, job.ID)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for manager.SlotWaiters(runs.ConnectionKey("conn_a")) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduled backup does not wait for the slot")
		}
		time.Sleep(time.Millisecond)
	}
	// Only the runs manager shuts down; the scheduler's context is still live.
	if err = manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-done

	list, err := metaStore.ListJobRuns(ctx, job.ID, 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("runs %v, %v", list, err)
	}
	if list[0].Status != models.JobRunCancelled {
		t.Fatalf("run status %s, want cancelled", list[0].Status)
	}
	rec, err := metaStore.GetBackupRecord(ctx, list[0].Databases[0].BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != models.StatusCancelled || rec.CancelledBy != runs.SystemActor || !strings.Contains(rec.ErrorMessage, "interrupted") {
		t.Fatalf("backup %s by %q: %q", rec.Status, rec.CancelledBy, rec.ErrorMessage)
	}
	pub.mu.Lock()
	for _, e := range pub.got {
		if e.Type == events.BackupFailed {
			t.Errorf("backup.failed published: %+v", e)
		}
	}
	pub.mu.Unlock()
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.runs) != 1 || observer.runs[0].Status != models.JobRunCancelled {
		t.Fatalf("observed runs %+v", observer.runs)
	}
}
