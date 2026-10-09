package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fullDiskStore fails to save the final (not in-progress) backup records, like
// the metadata database on a full data disk.
type fullDiskStore struct {
	*store.SQLiteStore
	full atomic.Bool
}

func (f *fullDiskStore) SaveBackupRecord(ctx context.Context, rec *models.BackupRecord) error {
	if f.full.Load() && rec.Status != models.StatusInProgress {
		return fmt.Errorf("store: save backup record %s: %w", rec.ID, errors.New("database or disk is full (13)"))
	}
	return f.SQLiteStore.SaveBackupRecord(ctx, rec)
}

// A backup whose final record cannot be saved never reports success: no
// backup.succeeded event, a failed run for the observer (the heartbeat sends
// /fail, not success), a system.disk_full alert, and the failed record saved
// once writes work again, its archive kept in storage.
func TestUnsavedFinalRecordIsNotASuccess(t *testing.T) {
	metaStore := &fullDiskStore{SQLiteStore: storetest.New(t)}
	metaStore.full.Store(true)
	mockStorage := storage.NewMockStorage()
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(runner))
	pub := &recordingPublisher{}
	observer := &finishedRuns{}
	guard := diskguard.New(diskguard.Config{Dir: t.TempDir(), Publisher: pub, SaveRetryDelays: []time.Duration{time.Millisecond},
		Free: func(string) (uint64, error) { return 1 << 40, nil }})
	sched := NewScheduler(metaStore, engine, mockStorage, nil, WithPublisher(pub), WithRunObserver(observer),
		WithRunRegistry(runs.NewRegistry()), WithDiskGuard(guard))

	ctx := context.Background()
	job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "@daily", Enabled: true}
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	rec, err := sched.TriggerJob(ctx, job.ID)
	if !errors.Is(err, diskguard.ErrNotSaved) {
		t.Fatalf("TriggerJob = %v, want ErrNotSaved", err)
	}
	if rec == nil || rec.Status != models.StatusFailed || rec.ArchiveCleanupPending {
		t.Fatalf("record %+v", rec)
	}

	pub.mu.Lock()
	var types []events.EventType
	for _, e := range pub.got {
		types = append(types, e.Type)
		if e.Type == events.BackupSucceeded || e.Type == events.VerificationSucceeded {
			t.Errorf("%s published for an unsaved backup", e.Type)
		}
	}
	pub.mu.Unlock()
	if !containsType(types, events.BackupFailed) || !containsType(types, events.SystemDiskFull) {
		t.Fatalf("events %v, want backup.failed and system.disk_full", types)
	}
	observer.mu.Lock()
	if len(observer.runs) != 1 || observer.runs[0].Status != models.JobRunFailed {
		t.Fatalf("observed runs %+v, want one failed run", observer.runs)
	}
	observer.mu.Unlock()

	// Nothing final is stored until writes work again.
	if stored, getErr := metaStore.GetBackupRecord(ctx, rec.ID); getErr == nil && stored.Status != models.StatusInProgress {
		t.Fatalf("stored %+v while the disk was full", stored)
	}
	metaStore.full.Store(false)
	_ = guard.Check()
	guard.RetryDeferred(ctx)
	stored, err := metaStore.GetBackupRecord(ctx, rec.ID)
	if err != nil || stored.Status != models.StatusFailed || stored.ArchiveCleanupPending || !strings.Contains(stored.ErrorMessage, models.ArchiveKeptNote) ||
		!strings.HasPrefix(stored.ErrorMessage, models.ErrRecordNotSaved) {
		t.Fatalf("settled record %+v, %v", stored, err)
	}
}

// A scheduled run that the space guard refuses fails (so its alerts fire)
// instead of being skipped silently.
func TestScheduledRunRefusedForSpaceFails(t *testing.T) {
	metaStore := storetest.New(t)
	manager := runs.NewManager(nil)
	guard := diskguard.New(diskguard.Config{Dir: "/data", MinFree: 100 << 20, Free: func(string) (uint64, error) { return 1 << 20, nil }})
	manager.SetAdmission(guard.Admit)
	pub := &recordingPublisher{}
	observer := &finishedRuns{}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017")
	sched := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithPublisher(pub), WithRunObserver(observer),
		WithBackupGuard(func(connectionID, database string) (func(), error) {
			return manager.Acquire(runs.BackupKey(connectionID, database))
		}))
	ctx := context.Background()
	job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "@daily", Enabled: true}
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	sched.executeJob(ctx, job.ID)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.got) != 1 || pub.got[0].Type != events.BackupFailed || !strings.Contains(pub.got[0].Error, "free space") {
		t.Fatalf("events %+v, want one backup.failed naming the free space", pub.got)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.runs) != 1 || observer.runs[0].Status != models.JobRunFailed {
		t.Fatalf("observed runs %+v", observer.runs)
	}
}

func containsType(list []events.EventType, t events.EventType) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}
