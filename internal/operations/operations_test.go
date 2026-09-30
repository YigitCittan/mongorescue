package operations_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func newService(t *testing.T) (*operations.Service, *store.SQLiteStore) {
	t.Helper()
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return operations.New(operations.Config{
		Store:   st,
		Backup:  backup.NewEngine(mock, "", backup.WithRunner(bRunner)),
		Restore: restore.NewEngine(mock, ""),
		Runs:    manager,
		Version: "v-test",
	}), st
}

func TestStatus(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	done := now.Add(-time.Hour)
	for _, j := range []*models.Job{{ID: "job_a", Name: "a", Database: "shop", Enabled: true}, {ID: "job_b", Name: "b", Database: "crm"}} {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*models.BackupRecord{
		{ID: "bkp_old", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-48 * time.Hour), SizeBytes: 5},
		{ID: "bkp_new", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-2 * time.Hour), CompletedAt: &done, SizeBytes: 7},
		{ID: "bkp_fail", JobID: "job_b", Database: "crm", Status: models.StatusFailed, StartedAt: now.Add(-time.Hour), ErrorMessage: "boom"},
		{ID: "bkp_oldfail", Database: "crm", Status: models.StatusFailed, StartedAt: now.Add(-30 * time.Hour)},
		{ID: "bkp_run", Database: "crm", Status: models.StatusInProgress, StartedAt: now},
	} {
		if err := st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	s, err := svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Health != "healthy" || s.Version != "v-test" || s.Jobs != 2 || s.RunningBackups != 1 || s.Stats.TotalBackups != 5 ||
		s.Stats.CompletedBackups != 2 || s.Stats.TotalBytes != 12 || s.Stats.ActiveJobs != 1 {
		t.Fatalf("status = %+v", s)
	}
	if len(s.FailedLast24h) != 1 || s.FailedLast24h[0].ID != "bkp_fail" || s.FailedLast24h[0].Error != "boom" {
		t.Fatalf("failed_last_24h = %+v", s.FailedLast24h)
	}
	for _, js := range s.JobStatus {
		switch js.ID {
		case "job_a":
			if js.LastSuccessBackupID != "bkp_new" || js.LastSuccessAt == nil || !js.LastSuccessAt.Equal(done) {
				t.Fatalf("job_a = %+v; want the newest completed backup", js)
			}
		case "job_b":
			if js.LastSuccessAt != nil {
				t.Fatalf("job_b never succeeded: %+v", js)
			}
		}
	}

	failed, err := svc.ListBackups(ctx, operations.BackupFilter{Database: "crm", Status: models.StatusFailed})
	if err != nil || len(failed) != 2 {
		t.Fatalf("filtered backups = %+v, %v", failed, err)
	}
}

func TestErrorsAreClassified(t *testing.T) {
	svc, _ := newService(t)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, Scope: auth.ScopeOperator})
	cases := []struct {
		err  error
		want error
		msg  string
	}{
		{func() error { _, err := svc.StartBackup(ctx, operations.BackupRequest{}); return err }(), operations.ErrConnectionRequired, "connection_id"},
		{func() error {
			_, err := svc.StartBackup(ctx, operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: "c"}})
			return err
		}(), operations.ErrUnknownConnection, "unknown connection_id"},
		{func() error { _, err := svc.RunJob(ctx, "job_x", models.TriggerOnDemand); return err }(), operations.ErrSchedulerUnavailable, "scheduler"},
		{func() error { _, err := svc.StartRestore(ctx, models.RestoreRequest{}); return err }(), operations.ErrInvalid, "backup_id required"},
		{func() error {
			f := false
			_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b", SafeClone: &f})
			return err
		}(), operations.ErrInvalid, "confirm_in_place"},
		{func() error {
			f := false
			_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b", SafeClone: &f, ConfirmInPlace: true})
			return err
		}(), auth.ErrForbidden, "admin"},
		{func() error { _, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "b"}); return err }(), operations.ErrNotFound, "source backup not found"},
		{func() error { _, err := svc.GetBackup(ctx, "b"); return err }(), operations.ErrNotFound, "backup not found"},
		{func() error { _, err := svc.GetRestore(ctx, "r"); return err }(), operations.ErrNotFound, "restore not found"},
		{func() error { _, err := svc.GetJob(ctx, "j"); return err }(), operations.ErrNotFound, "job not found"},
	}
	for i, tc := range cases {
		if !errors.Is(tc.err, tc.want) || !strings.Contains(tc.err.Error(), tc.msg) {
			t.Errorf("case %d: %v; want %v mentioning %q", i, tc.err, tc.want, tc.msg)
		}
	}
}

func TestLegacyBackupWithoutConnectionNeedsAnAdminToChooseTheTarget(t *testing.T) {
	svc, st := newService(t)
	if err := st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "bkp_legacy", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[auth.Scope]string{
		auth.ScopeOperator: "an admin must choose the target connection",
		auth.ScopeAdmin:    "choose the target connection (target_connection_id)",
	} {
		ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, Scope: scope})
		_, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_legacy"})
		if !errors.Is(err, operations.ErrConnectionRequired) || !strings.Contains(err.Error(), "no source connection recorded") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s restore of a legacy backup = %v; want ErrConnectionRequired saying %q", scope, err, want)
		}
	}
}

// fakeConnections resolves the connections in its map.
type fakeConnections map[string]*models.Connection

func (f fakeConnections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if c, ok := f[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, connections.ErrNotFound
}

func (f fakeConnections) Get(ctx context.Context, id string) (*models.Connection, error) {
	return f.Resolve(ctx, id)
}

func (f fakeConnections) List(context.Context) ([]*models.Connection, error) { return nil, nil }

// retryFixture is a service with one connection whose mongodump invocations are recorded.
type retryFixture struct {
	svc  *operations.Service
	st   *store.SQLiteStore
	runs *runs.Manager
	mu   sync.Mutex
	args [][]string
}

func newRetryFixture(t *testing.T) *retryFixture {
	t.Helper()
	f := &retryFixture{st: storetest.New(t)}
	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		f.mu.Lock()
		f.args = append(f.args, args)
		f.mu.Unlock()
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	mock := storage.NewMockStorage()
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	f.runs = manager
	f.svc = operations.New(operations.Config{
		Store:       f.st,
		Backup:      backup.NewEngine(mock, "", backup.WithRunner(runner)),
		Restore:     restore.NewEngine(mock, ""),
		Runs:        manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "primary", URI: "mongodb://u:pw@db.internal/"}},
	})
	return f
}

// save stores records.
func (f *retryFixture) save(t *testing.T, records ...*models.BackupRecord) {
	t.Helper()
	for _, r := range records {
		if err := f.st.SaveBackupRecord(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

// await polls backup id until it leaves the in-progress state and its run has released
// the database, so that a following backup of the same database is not refused as busy.
// The record is persisted before the run lock is released, so the status alone is not
// enough. It fails the test if the run does not finish within the deadline.
func (f *retryFixture) await(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := f.svc.GetBackup(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != models.StatusInProgress && !f.runs.Running(runs.BackupKey(rec.ConnectionID, rec.Database)) {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s still running after 5s: %+v", id, rec)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastArgs returns the arguments of the latest mongodump invocation.
func (f *retryFixture) lastArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.args) == 0 {
		return nil
	}
	return f.args[len(f.args)-1]
}

func TestRetryBackupRepeatsTheFailedBackupAndKeepsItsRecord(t *testing.T) {
	f := newRetryFixture(t)
	ctx := context.Background()
	failedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	original := &models.BackupRecord{
		ID: "bkp_failed", Trigger: models.TriggerManual, Database: "shop", ConnectionID: "conn_a", ConnectionName: "primary",
		Status: models.StatusFailed, StorageType: models.StorageLocal, StorageKey: "shop/2026/09/bkp_failed.archive",
		Collections: []string{"orders"}, StartedAt: failedAt.Add(-time.Minute), CompletedAt: &failedAt,
		DurationSeconds: 60, ErrorMessage: "start mongodump: mongodump not found",
	}
	f.save(t, original)

	rec, err := f.svc.RetryBackup(ctx, "bkp_failed", models.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == original.ID || rec.RetryOf != original.ID || rec.Database != "shop" || rec.ConnectionID != "conn_a" ||
		rec.Trigger != models.TriggerManual || !slices.Equal(rec.Collections, []string{"orders"}) ||
		strings.HasSuffix(rec.StorageKey, ".gz") || rec.Status != models.StatusInProgress {
		t.Fatalf("retry = %+v; want an in-progress copy of the failed backup", rec)
	}
	if done := f.await(t, rec.ID); done.Status != models.StatusCompleted || done.RetryOf != original.ID {
		t.Fatalf("retry outcome = %+v; want completed with retry_of", done)
	}
	if args := f.lastArgs(); !slices.Contains(args, "--collection=orders") || slices.Contains(args, "--gzip") {
		t.Fatalf("mongodump args = %v; want the original collection without gzip", args)
	}

	kept, err := f.svc.GetBackup(ctx, "bkp_failed")
	if err != nil {
		t.Fatal(err)
	}
	if kept.Status != models.StatusFailed || kept.ErrorMessage != original.ErrorMessage || kept.CompletedAt == nil ||
		!kept.CompletedAt.Equal(failedAt) || kept.RetryOf != "" {
		t.Fatalf("original after retry = %+v; want it unchanged", kept)
	}

	// A failed retry can itself be retried, forming a chain.
	f.save(t, &models.BackupRecord{ID: "bkp_failed_retry", Database: "shop", ConnectionID: "conn_a", Status: models.StatusFailed, RetryOf: "bkp_failed"})
	again, err := f.svc.RetryBackup(ctx, "bkp_failed_retry", models.TriggerMCP)
	if err != nil || again.RetryOf != "bkp_failed_retry" || again.Trigger != models.TriggerMCP {
		t.Fatalf("retry of a retry = %+v, %v", again, err)
	}
	f.await(t, again.ID)
}

func TestRetryOfAJobBackupUsesTheJobOptions(t *testing.T) {
	f := newRetryFixture(t)
	ctx := context.Background()
	if err := f.st.SaveJob(ctx, &models.Job{ID: "job_a", Name: "nightly", Database: "shop", ConnectionID: "conn_a", Gzip: true, ExcludeCollections: []string{"logs"}}); err != nil {
		t.Fatal(err)
	}
	f.save(t, &models.BackupRecord{ID: "bkp_job_failed", JobID: "job_a", Trigger: models.TriggerScheduled, Database: "shop", ConnectionID: "conn_a", Status: models.StatusFailed})

	rec, err := f.svc.RetryBackup(ctx, "bkp_job_failed", models.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if rec.JobID != "job_a" || rec.Trigger != models.TriggerManual || !strings.HasSuffix(rec.StorageKey, ".archive.gz") {
		t.Fatalf("retry = %+v; want a manual backup of the job's database, compressed", rec)
	}
	f.await(t, rec.ID)
	if args := f.lastArgs(); !slices.Contains(args, "--excludeCollection=logs") || !slices.Contains(args, "--gzip") {
		t.Fatalf("mongodump args = %v; want the job's exclusions and gzip", args)
	}
}

func TestRetryBackupRefusals(t *testing.T) {
	f := newRetryFixture(t)
	ctx := context.Background()
	f.save(t,
		&models.BackupRecord{ID: "bkp_ok", Database: "shop", ConnectionID: "conn_a", Status: models.StatusCompleted},
		&models.BackupRecord{ID: "bkp_running", Database: "shop", ConnectionID: "conn_a", Status: models.StatusInProgress},
		&models.BackupRecord{ID: "bkp_gone_conn", Database: "shop", ConnectionID: "conn_deleted", Status: models.StatusFailed},
		&models.BackupRecord{ID: "bkp_no_conn", Database: "shop", Status: models.StatusFailed},
	)
	for _, tc := range []struct {
		id   string
		want error
		msg  string
	}{
		{"bkp_missing", operations.ErrNotFound, "backup not found"},
		{"bkp_ok", operations.ErrNotRetryable, "only failed backups can be retried"},
		{"bkp_running", operations.ErrNotRetryable, "in_progress"},
		{"bkp_gone_conn", operations.ErrRetryUnavailable, "connection of backup bkp_gone_conn no longer exists"},
		{"bkp_no_conn", operations.ErrRetryUnavailable, "no source connection recorded"},
	} {
		rec, err := f.svc.RetryBackup(ctx, tc.id, models.TriggerManual)
		if rec != nil || !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("retry %s = %+v, %v; want %v mentioning %q", tc.id, rec, err, tc.want, tc.msg)
		}
	}
	if list, err := f.svc.ListBackups(ctx, operations.BackupFilter{}); err != nil || len(list) != 4 {
		t.Fatalf("backups after refused retries = %d, %v; want no new records", len(list), err)
	}
}
