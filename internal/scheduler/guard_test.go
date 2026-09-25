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

	if _, _, err := s.PrepareJobRun(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown job: got %v", err)
	}
	_ = metaStore.SaveJob(context.Background(), &models.Job{ID: "j", Database: "shop", CronExpression: "@daily"})
	job, record, err := s.PrepareJobRun(context.Background(), "j")
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
