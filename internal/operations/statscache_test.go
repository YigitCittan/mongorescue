package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// statsCountingStore counts the backup aggregations of Stats; fail is the error
// of the next one. It keeps the store's change stamp.
type statsCountingStore struct {
	*store.SQLiteStore
	calls atomic.Int32
	mu    sync.Mutex
	fail  error
}

func (c *statsCountingStore) BackupStats(ctx context.Context, since time.Time) (*store.BackupStats, error) {
	c.calls.Add(1)
	c.mu.Lock()
	err := c.fail
	c.fail = nil
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.SQLiteStore.BackupStats(ctx, since)
}

// unstampedStore hides the change stamp: the cache then relies on the TTL and
// the events.
type unstampedStore struct {
	store.Store
	calls *atomic.Int32
}

func (u unstampedStore) BackupStats(ctx context.Context, since time.Time) (*store.BackupStats, error) {
	u.calls.Add(1)
	return u.Store.BackupStats(ctx, since)
}

func newStatsService(t *testing.T, st store.Store, ttl time.Duration) *operations.Service {
	t.Helper()
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return operations.New(operations.Config{
		Store: st, Backup: backup.NewEngine(mock, "", backup.WithRunner(bRunner)), Restore: restore.NewEngine(mock, ""),
		Runs: manager, Version: "v-test", StatsCacheTTL: ttl,
		Storage: func(context.Context, string) (storage.Storage, error) { return mock, nil },
	})
}

// TestStatsCacheFollowsEveryWrite checks that the cached statistics are shared
// per connection access and follow every write to what they count, made through
// the service or straight to the store (the scheduler's writes), deletions
// included, without an event.
func TestStatsCacheFollowsEveryWrite(t *testing.T) {
	st := storetest.New(t)
	cs := &statsCountingStore{SQLiteStore: st}
	svc := newStatsService(t, cs, time.Hour)
	ctx := admin()
	now := time.Now().UTC()
	for _, j := range []*models.Job{
		{ID: "job_1", Name: "a", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: "c1"},
		{ID: "job_2", Name: "b", Database: "crm", CronExpression: "@daily", Enabled: true, ConnectionID: "c2"},
	} {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b_c2", JobID: "job_2", Database: "crm", ConnectionID: "c2",
		Status: models.StatusCompleted, StartedAt: now.Add(-time.Hour), SizeBytes: 7}); err != nil {
		t.Fatal(err)
	}

	first := svc.Stats(ctx)
	if again := svc.Stats(ctx); cs.calls.Load() != 1 || again.TotalBackups != first.TotalBackups {
		t.Fatalf("second call: %d computations; want the cached figures", cs.calls.Load())
	}
	// Another connection access is another entry with its own figures.
	limited := limitedTo(auth.ScopeRead, "c1")
	if l := svc.Stats(limited); l.TotalBackups != 0 || l.ActiveJobs != 1 || len(l.JobLastBackups) != 0 {
		t.Fatalf("limited to c1: %d backups, %d active jobs, last backups %v", l.TotalBackups, l.ActiveJobs, l.JobLastBackups)
	}
	if first.TotalBackups != 1 || first.ActiveJobs != 2 {
		t.Fatalf("admin figures: %+v", first)
	}
	if again := svc.Stats(ctx); again.TotalBackups != 1 || cs.calls.Load() != 1 {
		t.Fatalf("admin after a limited caller: %d backups (%d computations); want its own cached figures", again.TotalBackups, cs.calls.Load())
	}

	steps := []struct {
		name  string
		write func() error
		check func(operations.Stats) bool
	}{
		{"a backup started by the scheduler", func() error {
			return st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b_run", JobID: "job_1", Database: "shop", ConnectionID: "c1",
				Status: models.StatusInProgress, StartedAt: now})
		}, func(s operations.Stats) bool { return s.ActiveBackups == 1 && s.JobLastBackups["job_1"].ID == "b_run" }},
		{"the backup failing", func() error {
			return st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b_run", JobID: "job_1", Database: "shop", ConnectionID: "c1",
				Status: models.StatusFailed, StartedAt: now})
		}, func(s operations.Stats) bool { return s.ActiveBackups == 0 && s.FailedBackups == 1 }},
		{"a restore", func() error {
			return st.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "r1", BackupID: "b_c2", SourceDatabase: "crm", TargetDatabase: "crm_rescue",
				Status: models.RestoreStatusInProgress, StartedAt: now})
		}, func(s operations.Stats) bool { return s.ActiveRestores == 1 }},
		{"a job paused by the scheduler", func() error {
			return st.SaveJob(ctx, &models.Job{ID: "job_1", Name: "a", Database: "shop", CronExpression: "@daily", ConnectionID: "c1"})
		}, func(s operations.Stats) bool { return s.ActiveJobs == 1 }},
		{"a backup deleted through the service", func() error {
			_, err := svc.DeleteBackup(ctx, "b_c2", "test")
			return err
		}, func(s operations.Stats) bool { return s.JobLastBackups["job_2"].Status == models.StatusDeleted }},
		{"a job deleted in the store", func() error { return st.DeleteJob(ctx, "job_2") },
			func(s operations.Stats) bool { return s.ActiveJobs == 0 }},
	}
	for _, step := range steps {
		before := cs.calls.Load()
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if s := svc.Stats(ctx); !step.check(s) || cs.calls.Load() != before+1 {
			t.Errorf("after %s: %+v (%d computations); want fresh figures", step.name, s, cs.calls.Load()-before)
		}
	}
}

// TestStatsCacheWithoutStampUsesEvents checks that without a change stamp the
// events and InvalidateHistory drop the cached statistics.
func TestStatsCacheWithoutStampUsesEvents(t *testing.T) {
	st := storetest.New(t)
	calls := &atomic.Int32{}
	svc := newStatsService(t, unstampedStore{Store: st, calls: calls}, time.Hour)
	ctx := admin()
	svc.Stats(ctx)
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", Database: "shop", Status: models.StatusCompleted, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if s := svc.Stats(ctx); s.TotalBackups != 0 || calls.Load() != 1 {
		t.Fatalf("without a stamp or an event: %d backups, %d computations; want the cached figures", s.TotalBackups, calls.Load())
	}
	svc.HandleEvent(ctx, events.Event{Type: events.NotificationTest})
	if svc.Stats(ctx); calls.Load() != 1 {
		t.Fatal("an unrelated event dropped the cached figures")
	}
	svc.HandleEvent(ctx, events.Event{Type: events.BackupSucceeded})
	if s := svc.Stats(ctx); s.TotalBackups != 1 || calls.Load() != 2 {
		t.Fatalf("after backup.succeeded: %d backups (%d computations)", s.TotalBackups, calls.Load())
	}
	svc.InvalidateHistory()
	if svc.Stats(ctx); calls.Load() != 3 {
		t.Fatal("InvalidateHistory kept the cached figures")
	}
}

// TestStatsCacheComputesOnceAndNeverKeepsDegraded checks single flight among
// concurrent callers and that degraded figures are not shared.
func TestStatsCacheComputesOnceAndNeverKeepsDegraded(t *testing.T) {
	st := storetest.New(t)
	cs := &statsCountingStore{SQLiteStore: st}
	svc := newStatsService(t, cs, time.Hour)
	cs.fail = errors.New("disk on fire")
	if s := svc.Stats(admin()); !s.Degraded {
		t.Fatal("the failure did not degrade the figures")
	}
	if s := svc.Stats(admin()); s.Degraded || cs.calls.Load() != 2 {
		t.Fatalf("after a degraded computation: degraded %v, %d computations", s.Degraded, cs.calls.Load())
	}
	svc.InvalidateHistory()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { svc.Stats(admin()) })
	}
	wg.Wait()
	if n := cs.calls.Load(); n != 3 {
		t.Fatalf("%d computations for 16 callers; want 1 more", n-2)
	}
	// Without a TTL every call computes.
	plain := newStatsService(t, cs, 0)
	plain.Stats(admin())
	plain.Stats(admin())
	if n := cs.calls.Load(); n != 5 {
		t.Fatalf("without the cache: %d computations; want 2 more", n-3)
	}
}
