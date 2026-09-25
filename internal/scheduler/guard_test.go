package scheduler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestScheduledRunSkippedWhileDatabaseBusy(t *testing.T) {
	metaStore := storetest.New(t)
	var dumps atomic.Int32
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		dumps.Add(1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))

	busy := true
	var released atomic.Int32
	guard := func(_, database string) (func(), error) {
		if database != "shop" {
			t.Errorf("guard called for %q", database)
		}
		if busy {
			return nil, errors.New("busy")
		}
		return func() { released.Add(1) }, nil
	}
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithBackupGuard(guard))
	job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "@daily"}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	s.executeJob(context.Background(), job.ID)
	if dumps.Load() != 0 {
		t.Fatal("a scheduled run must be skipped while another backup of the database runs")
	}

	busy = false
	s.executeJob(context.Background(), job.ID)
	if dumps.Load() != 1 || released.Load() != 1 {
		t.Fatalf("dumps=%d released=%d; want 1/1", dumps.Load(), released.Load())
	}
}

func TestPrepareAndExecuteJobRun(t *testing.T) {
	metaStore := storetest.New(t)
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil)

	if _, _, err := s.PrepareJobRun(context.Background(), "missing", models.TriggerOnDemand); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job: got %v", err)
	}
	_ = metaStore.SaveJob(context.Background(), &models.Job{ID: "j", Database: "shop", CronExpression: "@daily"})
	job, record, err := s.PrepareJobRun(context.Background(), "j", models.TriggerOnDemand)
	if err != nil || record.Status != models.StatusInProgress || record.JobID != "j" {
		t.Fatalf("PrepareJobRun: %+v, %v", record, err)
	}
	final, err := s.ExecuteJobRun(context.Background(), job, record)
	if err != nil || final.ID != record.ID || final.Status != models.StatusCompleted {
		t.Fatalf("ExecuteJobRun: %+v, %v", final, err)
	}
	if stored, _ := metaStore.GetBackupRecord(context.Background(), record.ID); stored == nil || stored.Status != models.StatusCompleted {
		t.Fatalf("final record not persisted: %+v", stored)
	}
}

// orderCheckStore records whether a final backup record was saved before its job's
// run timestamps.
type orderCheckStore struct {
	store.Store
	early atomic.Int32
}

func (s *orderCheckStore) SaveBackupRecord(ctx context.Context, rec *models.BackupRecord) error {
	if rec.Status != models.StatusInProgress && rec.JobID != "" {
		if job, err := s.GetJob(ctx, rec.JobID); err == nil && job.LastRun == nil {
			s.early.Add(1)
		}
	}
	return s.Store.SaveBackupRecord(ctx, rec)
}

// TestJobTimestampsArePersistedBeforeTheFinalRecord proves a client that sees a job
// run's final status also sees the job's LastRun (no read-your-writes race).
func TestJobTimestampsArePersistedBeforeTheFinalRecord(t *testing.T) {
	st := &orderCheckStore{Store: storetest.New(t)}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	s := NewScheduler(st, backup.NewEngine(storage.NewMockStorage(), "mongodb://h", backup.WithRunner(runner)), storage.NewMockStorage(), nil)
	if err := st.SaveJob(context.Background(), &models.Job{ID: "job_order", Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TriggerJob(context.Background(), "job_order"); err != nil {
		t.Fatal(err)
	}
	if st.early.Load() != 0 {
		t.Fatal("the final backup record was saved before the job's LastRun")
	}
}
