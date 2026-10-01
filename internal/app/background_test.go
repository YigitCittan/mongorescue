package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// startedApp returns a started App on a fresh data directory.
func startedApp(t *testing.T) *App {
	t.Helper()
	application, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })
	if err = application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(application.Stop)
	return application
}

func TestAppRunHooks(t *testing.T) {
	a := startedApp(t)
	if got := a.ActiveRuns(); len(got) != 0 {
		t.Fatalf("ActiveRuns() = %v; want none", got)
	}
	release := make(chan struct{})
	key := runs.BackupKey("c", "shop")
	if err := a.runs.Go(key, func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := a.ActiveRuns(); len(got) != 1 || got[0] != key {
		t.Fatalf("ActiveRuns() = %v; want [%s]", got, key)
	}

	a.PauseRuns()
	if !a.scheduler.Paused() {
		t.Fatal("PauseRuns did not pause the scheduler")
	}
	if _, err := a.runs.Acquire(runs.RestoreKey("c", "other")); !errors.Is(err, runs.ErrShuttingDown) {
		t.Fatalf("a new run while paused: %v; want ErrShuttingDown", err)
	}
	a.ResumeRuns()
	if a.scheduler.Paused() {
		t.Fatal("ResumeRuns did not resume the scheduler")
	}
	release2, err := a.runs.Acquire(runs.RestoreKey("c", "other"))
	if err != nil {
		t.Fatalf("a new run after ResumeRuns: %v", err)
	}
	release2()

	if !a.Busy() {
		t.Fatal("Busy() = false while a backup runs")
	}
	close(release)
	if err := waitNotBusy(a); err != nil {
		t.Fatal(err)
	}
}

// waitNotBusy waits up to a second for a to be idle.
func waitNotBusy(a *App) error {
	deadline := time.Now().Add(time.Second)
	for a.Busy() {
		if time.Now().After(deadline) {
			return errors.New("the run did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

func TestAppForceStopRecordsTheReason(t *testing.T) {
	a := startedApp(t)
	ctx := context.Background()
	const reason = "cancelled: application force quit"

	backup := &models.BackupRecord{ID: "bk_running", Database: "shop", Status: models.StatusInProgress}
	restore := &models.RestoreRecord{ID: "rs_running", TargetDatabase: "shop_rescue", Status: models.RestoreStatusInProgress}
	done := &models.BackupRecord{ID: "bk_done", Database: "shop", Status: models.StatusCompleted}
	for _, b := range []*models.BackupRecord{backup, done} {
		if err := a.metaStore.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.metaStore.SaveRestoreRecord(ctx, restore); err != nil {
		t.Fatal(err)
	}
	// The tracked backup sees the force quit as a cancellation with the reason, like
	// the backup engine; the untracked restore is only cancelled by the shutdown and
	// saves itself as failed.
	tracked, err := a.registry.Register(runs.Meta{Kind: models.RunBackup, ID: backup.ID, Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	var cancellation *runs.Cancellation
	if err := a.runs.Go(runs.BackupKey("c", "shop"), func(runCtx context.Context) {
		defer tracked.End()
		runCtx = tracked.Bind(runCtx)
		<-runCtx.Done()
		cancellation = runs.CancellationOf(runCtx)
		backup.Status, backup.ErrorMessage = models.StatusCancelled, "backup cancelled: context canceled"
		_ = a.metaStore.SaveBackupRecord(context.WithoutCancel(runCtx), backup)
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.runs.Go(runs.RestoreKey("c", "shop_rescue"), func(runCtx context.Context) {
		<-runCtx.Done()
		restore.Status, restore.ErrorMessage = models.RestoreStatusFailed, "restore aborted: context canceled"
		_ = a.metaStore.SaveRestoreRecord(context.WithoutCancel(runCtx), restore)
	}); err != nil {
		t.Fatal(err)
	}

	a.ForceStop(reason)

	if got := a.ActiveRuns(); len(got) != 0 {
		t.Fatalf("runs still active after ForceStop: %v", got)
	}
	b, err := a.metaStore.GetBackupRecord(ctx, backup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := reason + " (backup cancelled: context canceled)"; b.Status != models.StatusCancelled || b.ErrorMessage != want ||
		b.CancelledBy != runs.SystemActor || b.CancelledAt == nil {
		t.Fatalf("cancelled backup = %s %q by %q; want cancelled %q by the system", b.Status, b.ErrorMessage, b.CancelledBy, want)
	}
	if cancellation == nil || cancellation.Reason != reason || cancellation.By != runs.SystemActor {
		t.Fatalf("the tracked run saw %+v; want the force quit as its cancellation", cancellation)
	}
	r, err := a.metaStore.GetRestoreRecord(ctx, restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := reason + " (restore aborted: context canceled)"; r.Status != models.RestoreStatusCancelled || r.ErrorMessage != want ||
		r.CancelledBy != runs.SystemActor {
		t.Fatalf("cancelled restore = %s %q; want cancelled %q", r.Status, r.ErrorMessage, want)
	}
	if d, _ := a.metaStore.GetBackupRecord(ctx, done.ID); d.Status != models.StatusCompleted || d.ErrorMessage != "" {
		t.Fatalf("completed backup modified: %+v", d)
	}
	// ForceStop and Stop are idempotent.
	a.ForceStop(reason)
	a.Stop()
}

func TestWithReason(t *testing.T) {
	const reason = "cancelled: application force quit"
	cases := map[string]string{
		"":                             reason,
		reason:                         reason,
		reason + " (x)":                reason + " (x)",
		"backup cancelled: ctx closed": reason + " (backup cancelled: ctx closed)",
	}
	for detail, want := range cases {
		if got := withReason(reason, detail); got != want {
			t.Errorf("withReason(%q) = %q; want %q", detail, got, want)
		}
	}
}
