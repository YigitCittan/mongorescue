package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// slowDump is a fake mongodump: it streams small chunks until finish is closed (then
// it ends cleanly) or its context ends (then it fails, like a killed process).
type slowDump struct {
	ctx    context.Context
	finish <-chan struct{}
}

func (d *slowDump) Read(p []byte) (int, error) {
	select {
	case <-d.finish:
		return 0, io.EOF
	case <-d.ctx.Done():
		return 0, d.ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	n := min(len(p), 512)
	clear(p[:n])
	return n, nil
}

// startBackup runs a backup of database through a real engine on store, as the API
// does: tracked by the run registry, under the App's run manager, its final record
// saved when it ends. finish lets the fake mongodump end.
func startBackup(t *testing.T, a *App, store storage.Storage, database string, finish <-chan struct{}) *models.BackupRecord {
	t.Helper()
	runner := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(&slowDump{ctx: ctx, finish: finish}), strings.NewReader(""), func() error { return context.Cause(ctx) }, nil
	}
	engine := backup.NewEngine(store, "mongodb://db.internal:27017", backup.WithRunner(runner))
	opts := models.BackupOptions{Database: database}
	record, err := engine.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.metaStore.SaveBackupRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	run, err := a.registry.Register(runs.Meta{Kind: models.RunBackup, ID: record.ID, Database: database})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.runs.Go(runs.BackupKey("c", database), func(ctx context.Context) {
		defer run.End()
		running := *record
		final, _ := engine.Execute(run.Bind(ctx), opts, &running)
		_ = a.metaStore.SaveBackupRecord(context.WithoutCancel(ctx), final)
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

// TestDrainLetsRunningBackupsFinish proves a graceful shutdown refuses new runs and
// waits for a running backup, which completes with its archive stored.
func TestDrainLetsRunningBackupsFinish(t *testing.T) {
	a := startedApp(t)
	ctx := context.Background()
	mock := storage.NewMockStorage()
	finish := make(chan struct{})
	rec := startBackup(t, a, mock, "shop", finish)

	time.AfterFunc(300*time.Millisecond, func() { close(finish) })
	done := make(chan bool, 1)
	go func() { done <- a.Drain(ctx, 30*time.Second) }()
	// New runs are refused at once, while the running backup goes on.
	deadline := time.Now().Add(5 * time.Second)
	for !a.scheduler.Paused() {
		if time.Now().After(deadline) {
			t.Fatal("Drain did not pause the runs")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := a.runs.Go(runs.BackupKey("c", "crm"), func(context.Context) {}); !errors.Is(err, runs.ErrShuttingDown) {
		t.Fatalf("a new run while draining: %v; want ErrShuttingDown", err)
	}
	select {
	case idle := <-done:
		if !idle {
			t.Fatal("Drain reported runs left after the backup finished")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return after the backup finished")
	}
	a.Stop()
	got, err := a.metaStore.GetBackupRecord(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusCompleted || got.SizeBytes == 0 {
		t.Fatalf("backup = %s (%d bytes, %q); want completed", got.Status, got.SizeBytes, got.ErrorMessage)
	}
	if _, err = mock.Stat(ctx, got.StorageKey); err != nil {
		t.Fatalf("the completed archive is missing: %v", err)
	}
}

// TestShutdownCancelsBackupsAfterTheGracePeriod proves a backup still running when
// the grace period ends is cancelled cleanly: recorded as cancelled with the shutdown
// as its reason, its partial archive removed, never completed.
func TestShutdownCancelsBackupsAfterTheGracePeriod(t *testing.T) {
	a := startedApp(t)
	ctx := context.Background()
	a.cfg.ShutdownGrace = 200 * time.Millisecond
	mock := storage.NewMockStorage()
	never := make(chan struct{})
	rec := startBackup(t, a, mock, "shop", never)

	start := time.Now()
	if err := a.shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if waited := time.Since(start); waited < a.cfg.ShutdownGrace {
		t.Fatalf("shutdown returned after %s, before the grace period of %s", waited, a.cfg.ShutdownGrace)
	}
	if a.Busy() {
		t.Fatalf("runs left after shutdown: %v", a.ActiveRuns())
	}
	got, err := a.metaStore.GetBackupRecord(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusCancelled || !strings.HasPrefix(got.ErrorMessage, ShutdownReason) ||
		got.CancelledBy != runs.SystemActor || got.SizeBytes != 0 || got.SHA256 != "" {
		t.Fatalf("backup = %s %q by %q (%d bytes); want cancelled by the shutdown", got.Status, got.ErrorMessage, got.CancelledBy, got.SizeBytes)
	}
	if _, err = mock.Stat(ctx, got.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the partial archive of the cancelled backup was kept: %v", err)
	}
}

// TestShutdownWithoutGraceCancelsAtOnce proves the default (no grace period) does not
// wait.
func TestShutdownWithoutGraceCancelsAtOnce(t *testing.T) {
	a := startedApp(t)
	never := make(chan struct{})
	rec := startBackup(t, a, storage.NewMockStorage(), "shop", never)
	start := time.Now()
	if err := a.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("shutdown without a grace period took %s", waited)
	}
	got, err := a.metaStore.GetBackupRecord(context.Background(), rec.ID)
	if err != nil || got.Status != models.StatusCancelled {
		t.Fatalf("backup = %+v, %v; want cancelled", got, err)
	}
}
