package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestRetentionPruningDays(t *testing.T) {
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	ctx := context.Background()

	now := time.Now().UTC()

	// Create 3 backups: 1 old (30 days ago), 1 recent (5 days ago), 1 fresh (today)
	recOld := &models.BackupRecord{
		ID:         "bkp_old",
		Database:   "analytics",
		Trigger:    models.TriggerScheduled,
		Status:     models.StatusCompleted,
		StorageKey: "analytics/old.gz",
		StartedAt:  now.AddDate(0, 0, -30),
	}
	recRecent := &models.BackupRecord{
		ID:         "bkp_recent",
		Database:   "analytics",
		Trigger:    models.TriggerScheduled,
		Status:     models.StatusCompleted,
		StorageKey: "analytics/recent.gz",
		StartedAt:  now.AddDate(0, 0, -5),
	}
	recFresh := &models.BackupRecord{
		ID:         "bkp_fresh",
		Database:   "analytics",
		Trigger:    models.TriggerScheduled,
		Status:     models.StatusCompleted,
		StorageKey: "analytics/fresh.gz",
		StartedAt:  now,
	}

	for _, r := range []*models.BackupRecord{recOld, recRecent, recFresh} {
		_ = metaStore.SaveBackupRecord(ctx, r)
		_, _ = mockStorage.Save(ctx, r.StorageKey, strings.NewReader("dummy"))
	}

	// Prune with 14 days retention
	pruned, err := PruneBackups(ctx, 14, 0, []*models.BackupRecord{recOld, recRecent, recFresh}, metaStore, mockStorage, nil)
	if err != nil {
		t.Fatalf("unexpected prune error: %v", err)
	}

	if len(pruned) != 1 || pruned[0] != "bkp_old" {
		t.Fatalf("expected bkp_old to be pruned, got %v", pruned)
	}

	// Verify status in store is pruned
	updatedOld, _ := metaStore.GetBackupRecord(ctx, "bkp_old")
	if updatedOld.Status != models.StatusPruned {
		t.Errorf("expected status pruned, got %s", updatedOld.Status)
	}

	// Verify file was deleted from storage
	_, err = mockStorage.Retrieve(ctx, recOld.StorageKey)
	if err == nil {
		t.Error("expected physical file to be deleted from storage")
	}
}

func TestRetentionPruningCount(t *testing.T) {
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	ctx := context.Background()

	now := time.Now().UTC()

	var records []*models.BackupRecord
	for i := 1; i <= 5; i++ {
		r := &models.BackupRecord{
			ID:         "bkp_" + string(rune('0'+i)),
			Database:   "analytics",
			Trigger:    models.TriggerScheduled,
			Status:     models.StatusCompleted,
			StorageKey: "analytics/" + string(rune('0'+i)) + ".gz",
			StartedAt:  now.Add(-time.Duration(i) * 25 * time.Hour),
		}
		records = append(records, r)
		_ = metaStore.SaveBackupRecord(ctx, r)
		_, _ = mockStorage.Save(ctx, r.StorageKey, strings.NewReader("dummy"))
	}

	// Keep only 2 backups
	pruned, err := PruneBackups(ctx, 0, 2, records, metaStore, mockStorage, nil)
	if err != nil {
		t.Fatalf("unexpected prune error: %v", err)
	}

	if len(pruned) != 3 {
		t.Fatalf("expected 3 items pruned, got %d (%v)", len(pruned), pruned)
	}
}

func TestSchedulerLifecycle(t *testing.T) {
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()

	// Mock runner for backup engine
	customRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		stdout := io.NopCloser(bytes.NewReader([]byte("sample-archive")))
		stderr := strings.NewReader("done\n")
		return stdout, stderr, func() error { return nil }, nil
	}

	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(customRunner))
	sched := NewScheduler(metaStore, bEngine, mockStorage, nil)

	ctx := context.Background()

	job := &models.Job{
		ID:             "job_analytics",
		Name:           "Analytics Job",
		Database:       "analytics",
		CronExpression: "@daily",
		RetentionCount: 5,
		Enabled:        true,
	}

	_ = metaStore.SaveJob(ctx, job)

	if err := sched.Start(ctx); err != nil {
		t.Fatalf("failed to start scheduler: %v", err)
	}
	defer sched.Stop()

	// Trigger manual run
	rec, err := sched.TriggerJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}
	if rec.Status != models.StatusCompleted {
		t.Errorf("expected completed, got %s", rec.Status)
	}

	// Check job last run updated
	updatedJob, _ := metaStore.GetJob(ctx, job.ID)
	if updatedJob.LastRun == nil {
		t.Error("expected LastRun to be updated")
	}

	// Unregister
	sched.UnregisterJob(job.ID)
}

// ctxStrictStore fails writes whose context is already done, mimicking a real
// database driver, so tests can prove post-run persistence is detached from cancellation.
type ctxStrictStore struct {
	store.Store
}

func (s ctxStrictStore) SaveJob(ctx context.Context, job *models.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.SaveJob(ctx, job)
}

func (s ctxStrictStore) UpdateJob(ctx context.Context, job *models.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.UpdateJob(ctx, job)
}

func (s ctxStrictStore) SaveBackupRecord(ctx context.Context, rec *models.BackupRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.SaveBackupRecord(ctx, rec)
}

func newTestScheduler(t *testing.T) (*Scheduler, store.Store) {
	t.Helper()
	fileStore := storetest.New(t)
	metaStore := ctxStrictStore{Store: fileStore}
	mockStorage := storage.NewMockStorage()
	runner := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return ctx.Err() }, nil
	}
	engine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(runner))
	return NewScheduler(metaStore, engine, mockStorage, nil), metaStore
}

func TestSchedulerStartTwice(t *testing.T) {
	sched, _ := newTestScheduler(t)

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer sched.Stop()

	if err := sched.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start: expected ErrAlreadyStarted, got %v", err)
	}
}

func TestSchedulerStopIsSafe(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		sched, _ := newTestScheduler(t)
		sched.Stop()
		sched.Stop()
	})

	t.Run("after start, twice", func(t *testing.T) {
		sched, _ := newTestScheduler(t)
		if err := sched.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sched.Stop()
		sched.Stop()

		if err := sched.Start(context.Background()); !errors.Is(err, ErrStopped) {
			t.Fatalf("Start after Stop: expected ErrStopped, got %v", err)
		}
	})

	t.Run("cron callback after stop is a no-op", func(t *testing.T) {
		sched, _ := newTestScheduler(t)
		sched.Stop()
		done := make(chan struct{})
		go func() {
			sched.runScheduled("missing")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runScheduled blocked after Stop")
		}
	})
}

func TestCancelledRunIsPersistedAsFailed(t *testing.T) {
	sched, metaStore := newTestScheduler(t)
	job := &models.Job{
		ID:             "job_cancel",
		Database:       "analytics",
		CronExpression: "@daily",
		Enabled:        true,
	}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec, err := sched.runBackupForJob(ctx, job)
	if err == nil {
		t.Fatal("expected an error from a cancelled run")
	}
	if rec == nil {
		t.Fatal("expected a backup record for a cancelled run")
	}

	stored, getErr := metaStore.GetBackupRecord(context.Background(), rec.ID)
	if getErr != nil {
		t.Fatalf("cancelled run record was not persisted: %v", getErr)
	}
	if stored.Status != models.StatusFailed {
		t.Fatalf("expected persisted status %q, got %q", models.StatusFailed, stored.Status)
	}

	updated, _ := metaStore.GetJob(context.Background(), job.ID)
	if updated == nil || updated.LastRun == nil {
		t.Fatal("expected job LastRun to be persisted despite cancellation")
	}
}

func TestSchedulerStartAfterStopBeforeStart(t *testing.T) {
	sched, _ := newTestScheduler(t)
	sched.Stop()
	if err := sched.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("expected ErrStopped, got %v", err)
	}
}

// flakyListStore fails ListJobs a fixed number of times before delegating.
type flakyListStore struct {
	store.Store
	failures int
}

var errTransient = errors.New("transient store failure")

func (s *flakyListStore) ListJobs(ctx context.Context) ([]*models.Job, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errTransient
	}
	return s.Store.ListJobs(ctx)
}

func TestSchedulerStartRetryAfterTransientError(t *testing.T) {
	fileStore := storetest.New(t)
	flaky := &flakyListStore{Store: fileStore, failures: 1}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017")
	sched := NewScheduler(flaky, engine, storage.NewMockStorage(), nil)
	defer sched.Stop()

	if err := sched.Start(context.Background()); !errors.Is(err, errTransient) {
		t.Fatalf("first Start: expected transient error, got %v", err)
	}
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("retry Start: expected success, got %v", err)
	}
	if err := sched.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("third Start: expected ErrAlreadyStarted, got %v", err)
	}
}

// fakeTargets serves one mock driver per target ID.
type fakeTargets struct {
	drivers map[string]*storage.MockStorage
	def     string
}

func (f *fakeTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id == "" {
		id = f.def
	}
	if _, ok := f.drivers[id]; !ok {
		return nil, errors.New("unknown target")
	}
	return &models.StorageTarget{ID: id, Name: "target " + id, Type: models.StorageS3}, nil
}

func (f *fakeTargets) Storage(ctx context.Context, id string) (storage.Storage, error) {
	t, err := f.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return f.drivers[t.ID], nil
}

func TestJobRunsUseTheirTargetAndRetentionCountsPerTarget(t *testing.T) {
	metaStore := storetest.New(t)
	tg := &fakeTargets{def: "stg_a", drivers: map[string]*storage.MockStorage{"stg_a": storage.NewMockStorage(), "stg_b": storage.NewMockStorage()}}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("a"))), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(nil, "mongodb://h", backup.WithRunner(runner), backup.WithStorageResolver(tg.Storage))
	sched := NewScheduler(metaStore, engine, nil, nil, WithStorageTargets(tg))
	ctx := context.Background()

	// Older completed backups of the same database kept on targets a and b.
	oldA := &models.BackupRecord{ID: "bkp_old_a", JobID: "job_b", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/old_a",
		StorageTargetID: "stg_a", StartedAt: time.Now().Add(-48 * time.Hour)}
	oldB := &models.BackupRecord{ID: "bkp_old_b", JobID: "job_b", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/old_b",
		StorageTargetID: "stg_b", StartedAt: time.Now().Add(-48 * time.Hour)}
	for _, rec := range []*models.BackupRecord{oldA, oldB} {
		if _, err := tg.drivers[rec.StorageTargetID].Save(ctx, rec.StorageKey, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		if err := metaStore.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	job := &models.Job{ID: "job_b", Name: "b", Database: "shop", CronExpression: "@daily", RetentionCount: 1, StorageTargetID: "stg_b"}
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	first, err := sched.TriggerJob(ctx, job.ID)
	if err != nil || first.StorageTargetID != "stg_b" || first.StorageTargetName != "target stg_b" || first.StorageType != models.StorageS3 {
		t.Fatalf("run = %+v, %v", first, err)
	}
	if r, _ := metaStore.GetBackupRecord(ctx, oldB.ID); r.Status != models.StatusCompleted {
		t.Fatalf("an on-demand run pruned %s", oldB.ID)
	}
	time.Sleep(1100 * time.Millisecond) // backup IDs have second resolution
	sched.executeJob(ctx, job.ID)       // a scheduled run applies retention

	// Retention on target b pruned the old backup there (the on-demand run is under a
	// day old and kept), not the backup on target a.
	if r, _ := metaStore.GetBackupRecord(ctx, oldB.ID); r.Status != models.StatusPruned {
		t.Fatalf("old backup on b = %s; want pruned", r.Status)
	}
	if _, err := tg.drivers["stg_b"].Stat(ctx, oldB.StorageKey); err == nil {
		t.Fatal("pruned artifact still on target b")
	}
	if r, _ := metaStore.GetBackupRecord(ctx, oldA.ID); r.Status != models.StatusCompleted {
		t.Fatalf("backup on another target = %s; want untouched", r.Status)
	}
	if r, _ := metaStore.GetBackupRecord(ctx, first.ID); r.Status != models.StatusCompleted {
		t.Fatalf("recent on-demand run = %s; want kept", r.Status)
	}
}

func TestJobDeletedDuringARunIsNotRecreated(t *testing.T) {
	metaStore := storetest.New(t)
	ctx := context.Background()
	var deleted bool
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		if !deleted {
			deleted = true
			if err := metaStore.DeleteJob(ctx, "job_gone"); err != nil {
				t.Error(err)
			}
		}
		return io.NopCloser(bytes.NewReader([]byte("a"))), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://h", backup.WithRunner(runner))
	sched := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil)
	if err := metaStore.SaveJob(ctx, &models.Job{ID: "job_gone", Name: "g", Database: "db", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.TriggerJob(ctx, "job_gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := metaStore.GetJob(ctx, "job_gone"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job deleted during its run = %v; want ErrNotFound (not recreated)", err)
	}
}
