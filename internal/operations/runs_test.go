package operations_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// runFixture is a service whose backups block in a fake mongodump until cancelled.
type runFixture struct {
	svc      *operations.Service
	st       *store.SQLiteStore
	manager  *runs.Manager
	registry *runs.Registry
	logs     *runlog.Dir
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := &runFixture{st: storetest.New(t), logs: runlog.NewDir(t.TempDir())}
	blocking := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("partial archive"))
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, strings.NewReader("2026-10-01T10:00:00.000+0000\twriting shop.orders to archive on stdout\n"),
			func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
	mock := storage.NewMockStorage()
	f.manager = runs.NewManager(nil)
	t.Cleanup(func() { _ = f.manager.Shutdown(context.Background()) })
	f.registry = runs.NewRegistry(runs.WithLogs(f.logs))
	f.svc = operations.New(operations.Config{
		Store:       f.st,
		Backup:      backup.NewEngine(mock, "", backup.WithRunner(blocking)),
		Restore:     restore.NewEngine(mock, ""),
		Runs:        f.manager,
		Registry:    f.registry,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "primary", URI: "mongodb://u:pw@db.internal/"}},
		Settings: func() settings.Settings {
			s := settings.Defaults()
			s.General.LogRetentionDays = 1
			return s
		},
	})
	return f
}

func asUser(name string, scope auth.Scope) context.Context {
	p := &auth.Principal{User: &auth.User{ID: "usr_" + name, Username: name}, Method: auth.MethodSession, Scope: scope}
	return auth.WithPrincipal(context.Background(), p)
}

func asKey(name string, scope auth.Scope) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_1", APIKeyName: name, Scope: scope})
}

// awaitStatus polls backup id until it leaves the in-progress state and its run ended.
func (f *runFixture) awaitStatus(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		rec, err := f.svc.GetBackup(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != models.StatusInProgress && f.registry.Get(id) == nil {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s still running: %+v", id, rec)
		}
	}
}

func TestCancelBackupRecordsWhoAndIsNotAFailure(t *testing.T) {
	f := newRunFixture(t)
	started, err := f.svc.StartBackup(asUser("alice", auth.ScopeAdmin), operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a", Database: "shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The running backup carries its live progress in API results.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		list, listErr := f.svc.ListBackups(context.Background(), operations.BackupFilter{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(list) == 1 && list[0].Progress != nil && list[0].Progress.Phase == models.PhaseDumping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no live progress on the running backup: %+v", list[0])
		}
	}
	if active := f.svc.ActiveRuns(); len(active) != 1 || active[0].ID != started.ID || active[0].Kind != models.RunBackup {
		t.Fatalf("active runs = %+v", active)
	}

	rec, err := f.svc.CancelBackup(asKey("ci", auth.ScopeOperator), started.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != started.ID || rec.Status != models.StatusInProgress || rec.Progress == nil || !rec.Progress.Cancelling {
		t.Fatalf("cancel result = %+v", rec)
	}
	final := f.awaitStatus(t, started.ID)
	if final.Status != models.StatusCancelled || final.CancelledBy != "API key ci" || final.CancelledAt == nil ||
		final.ErrorMessage != "backup cancelled by API key ci" || final.Progress != nil {
		t.Fatalf("final = %+v", final)
	}
	if _, err = f.svc.CancelBackup(context.Background(), started.ID, ""); !errors.Is(err, operations.ErrNotRunning) {
		t.Fatalf("cancelling a cancelled backup = %v, want ErrNotRunning", err)
	}
	if _, err = f.svc.CancelBackup(context.Background(), "bkp_unknown", ""); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("cancelling an unknown backup = %v, want ErrNotFound", err)
	}
	if len(f.svc.ActiveRuns()) != 0 {
		t.Fatal("the cancelled run is still active")
	}

	// A cancelled backup is not a failure.
	st, err := f.svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Stats.FailedBackups != 0 || st.Stats.CancelledBackups != 1 || len(st.FailedLast24h) != 0 || st.RunningBackups != 0 {
		t.Fatalf("status = %+v", st)
	}

	// The run log is served and removed with the record.
	r, err := f.svc.OpenBackupLog(context.Background(), started.ID)
	if err != nil {
		t.Fatal(err)
	}
	tail, _ := r.Tail(50)
	_ = r.Close()
	if !strings.Contains(string(tail), "writing shop.orders") || !strings.Contains(string(tail), "cancelled by API key ci") {
		t.Fatalf("log = %q", tail)
	}
	f.svc.RemoveRunLog(started.ID)
	if _, err := f.svc.OpenBackupLog(context.Background(), started.ID); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("removed log = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.OpenBackupLog(context.Background(), "bkp_unknown"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("log of an unknown backup = %v", err)
	}
}

func TestCancelRefusesRunsThatAreNotActive(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	// A record left in progress without a run in this process (e.g. a crashed run that
	// is not yet marked interrupted) cannot be cancelled.
	stale := &models.BackupRecord{ID: "bkp_stale", Database: "shop", Status: models.StatusInProgress, StartedAt: time.Now()}
	done := &models.BackupRecord{ID: "bkp_done", Database: "shop", Status: models.StatusCompleted, StartedAt: time.Now()}
	for _, b := range []*models.BackupRecord{stale, done} {
		if err := f.st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"bkp_stale", "bkp_done"} {
		if _, err := f.svc.CancelBackup(ctx, id, ""); !errors.Is(err, operations.ErrNotRunning) {
			t.Fatalf("cancel %s = %v, want ErrNotRunning", id, err)
		}
	}

	inPlace := &models.RestoreRecord{ID: "rst_inplace", TargetDatabase: "shop", Status: models.RestoreStatusInProgress, InPlace: true, StartedAt: time.Now()}
	clone := &models.RestoreRecord{ID: "rst_clone", TargetDatabase: "shop_rescue", Status: models.RestoreStatusInProgress, StartedAt: time.Now()}
	for _, r := range []*models.RestoreRecord{inPlace, clone} {
		if err := f.st.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Stopping an in-place restore leaves partial data: operator keys may not.
	if _, err := f.svc.CancelRestore(asKey("ops", auth.ScopeOperator), "rst_inplace", ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator cancelling an in-place restore = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CancelRestore(asUser("admin", auth.ScopeAdmin), "rst_inplace", ""); !errors.Is(err, operations.ErrNotRunning) {
		t.Fatalf("admin cancelling an inactive in-place restore = %v, want ErrNotRunning", err)
	}
	if _, err := f.svc.CancelRestore(asKey("ops", auth.ScopeOperator), "rst_clone", ""); !errors.Is(err, operations.ErrNotRunning) {
		t.Fatalf("operator cancelling an inactive clone restore = %v, want ErrNotRunning", err)
	}
	if _, err := f.svc.CancelRun(ctx, "nothing", "MCP"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("CancelRun of an unknown id = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.CancelRun(ctx, "rst_clone", "MCP"); !errors.Is(err, operations.ErrNotRunning) {
		t.Fatalf("CancelRun of an inactive restore = %v, want ErrNotRunning", err)
	}
}

func TestCancelRunReachesTheRunningRestore(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	rec := &models.RestoreRecord{ID: "rst_live", TargetDatabase: "shop_rescue", Status: models.RestoreStatusInProgress, StartedAt: time.Now()}
	if err := f.st.SaveRestoreRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	run, err := f.registry.Register(runs.Meta{Kind: models.RunRestore, ID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	runCtx := run.Bind(ctx)
	defer run.End()
	res, err := f.svc.CancelRun(asKey("assistant", auth.ScopeOperator), rec.ID, "MCP")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != models.RunRestore || res.Restore == nil || res.Restore.ID != rec.ID {
		t.Fatalf("CancelRun = %+v", res)
	}
	<-runCtx.Done()
	if c := runs.CancellationOf(runCtx); c == nil || c.By != "API key assistant via MCP" || c.Kind != runs.ActorAPIKey {
		t.Fatalf("cancellation = %+v", c)
	}
}

func TestPruneRunLogsUsesTheSetting(t *testing.T) {
	f := newRunFixture(t)
	w, err := f.logs.Create("bkp_old")
	if err != nil {
		t.Fatal(err)
	}
	w.Printf("old")
	_ = w.Close()
	if n, err := f.svc.PruneRunLogs(); err != nil || n != 0 {
		t.Fatalf("a fresh log was pruned: %d, %v", n, err)
	}
}

func TestPauseJobUntil(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_p", Name: "p", Database: "shop", ConnectionID: "conn_a", CronExpression: "@daily", Enabled: true}
	if err := f.st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	update := func(enabled *bool, until *time.Time) (*models.Job, error) {
		return f.svc.UpdateJob(ctx, "job_p", operations.JobUpdate{
			Name: "p", Database: "shop", ConnectionID: "conn_a", CronExpression: "@daily", Enabled: enabled, PausedUntil: until,
		})
	}
	off, on := false, true
	past := time.Now().Add(-time.Hour)
	if _, err := update(&off, &past); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("pause until the past = %v, want ErrInvalid", err)
	}
	until := time.Now().Add(time.Hour).Round(time.Second)
	got, err := update(&off, &until)
	if err != nil || got.Enabled || got.PausedUntil == nil || !got.PausedUntil.Equal(until) {
		t.Fatalf("paused = %+v, %v", got, err)
	}
	// An edit that keeps the job paused keeps the time.
	if got, err = update(nil, nil); err != nil || got.PausedUntil == nil {
		t.Fatalf("edit while paused = %+v, %v", got, err)
	}
	if got, err = update(&on, nil); err != nil || !got.Enabled || got.PausedUntil != nil {
		t.Fatalf("resumed = %+v, %v", got, err)
	}
	if got, err = update(&off, nil); err != nil || got.Enabled || got.PausedUntil != nil {
		t.Fatalf("paused indefinitely = %+v, %v", got, err)
	}
}

func TestRenamingAJobWhosePauseHasPassed(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	// Paused until a minute ago: due to resume, but the scheduler has not run yet.
	until := time.Now().Add(-time.Minute).UTC().Round(time.Second)
	job := &models.Job{ID: "job_due", Name: "old", Database: "shop", ConnectionID: "conn_a", CronExpression: "@daily", PausedUntil: &until}
	if err := f.st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.UpdateJob(ctx, "job_due", operations.JobUpdate{Name: "renamed", Database: "shop", ConnectionID: "conn_a", CronExpression: "@daily"})
	if err != nil {
		t.Fatalf("renaming a job whose pause has passed = %v", err)
	}
	if got.Name != "renamed" || got.Enabled || got.PausedUntil == nil || !got.PausedUntil.Equal(until) {
		t.Fatalf("renamed job = %+v; want it still paused with its due time, for the scheduler to resume", got)
	}
	// A paused_until sent in the past is still refused.
	off := false
	past := time.Now().Add(-time.Hour)
	if _, err := f.svc.UpdateJob(ctx, "job_due", operations.JobUpdate{Name: "x", Database: "shop", ConnectionID: "conn_a", Enabled: &off, PausedUntil: &past}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("pause until the past = %v, want ErrInvalid", err)
	}
}

func TestCancelIsRefusedOnceTheRunIsFinishing(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	rec := &models.BackupRecord{ID: "bkp_fin", Database: "shop", Status: models.StatusInProgress, StartedAt: time.Now()}
	if err := f.st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	run, err := f.registry.Register(runs.Meta{Kind: models.RunBackup, ID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	runCtx := run.Bind(ctx)
	defer run.End()
	run.Finishing()
	_, err = f.svc.CancelBackup(ctx, rec.ID, "")
	if !errors.Is(err, operations.ErrNotRunning) || !strings.Contains(err.Error(), "already finishing") {
		t.Fatalf("cancelling a finishing backup = %v, want ErrNotRunning (already finishing)", err)
	}
	if runCtx.Err() != nil {
		t.Fatal("a finishing run must not be cancelled")
	}
}
