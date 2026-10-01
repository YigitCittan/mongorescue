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
	failing := &models.BackupRecord{ID: "bk_failing", Database: "crm", Status: models.StatusInProgress}
	restore := &models.RestoreRecord{ID: "rs_running", TargetDatabase: "shop_rescue", Status: models.RestoreStatusInProgress}
	untracked := &models.RestoreRecord{ID: "rs_untracked", TargetDatabase: "crm_rescue", Status: models.RestoreStatusInProgress}
	done := &models.BackupRecord{ID: "bk_done", Database: "shop", Status: models.StatusCompleted}
	for _, b := range []*models.BackupRecord{backup, failing, done} {
		if err := a.metaStore.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*models.RestoreRecord{restore, untracked} {
		if err := a.metaStore.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// track starts a tracked run whose engine stand-in saves save() once its context
	// is done, and returns the cancellation the run saw.
	track := func(kind models.RunKind, id, key string, save func(context.Context)) *runs.Cancellation {
		run, err := a.registry.Register(runs.Meta{Kind: kind, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		var seen runs.Cancellation
		if err := a.runs.Go(key, func(runCtx context.Context) {
			defer run.End()
			runCtx = run.Bind(runCtx)
			<-runCtx.Done()
			if c := runs.CancellationOf(runCtx); c != nil {
				seen = *c
			}
			save(context.WithoutCancel(runCtx))
		}); err != nil {
			t.Fatal(err)
		}
		return &seen
	}
	// Like the engines: a cancelled run records itself as cancelled...
	seen := track(models.RunBackup, backup.ID, runs.BackupKey("c", "shop"), func(ctx context.Context) {
		backup.Status, backup.ErrorMessage = models.StatusCancelled, "backup cancelled: application force quit"
		_ = a.metaStore.SaveBackupRecord(ctx, backup)
	})
	_ = track(models.RunRestore, restore.ID, runs.RestoreKey("c", "shop_rescue"), func(ctx context.Context) {
		restore.Status, restore.ErrorMessage = models.RestoreStatusCancelled, "restore cancelled: application force quit"
		_ = a.metaStore.SaveRestoreRecord(ctx, restore)
	})
	// ...and one that failed on its own keeps its failure.
	_ = track(models.RunBackup, failing.ID, runs.BackupKey("c", "crm"), func(ctx context.Context) {
		failing.Status, failing.ErrorMessage = models.StatusFailed, "backup: mongodump failed: exit status 1"
		_ = a.metaStore.SaveBackupRecord(ctx, failing)
	})
	// A run the registry does not know is only stopped by the shutdown.
	if err := a.runs.Go(runs.RestoreKey("c", "crm_rescue"), func(runCtx context.Context) {
		<-runCtx.Done()
		untracked.Status, untracked.ErrorMessage = models.RestoreStatusFailed, "restore aborted: context canceled"
		_ = a.metaStore.SaveRestoreRecord(context.WithoutCancel(runCtx), untracked)
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
	if want := reason + " (backup cancelled: application force quit)"; b.Status != models.StatusCancelled || b.ErrorMessage != want ||
		b.CancelledBy != runs.SystemActor || b.CancelledAt == nil {
		t.Fatalf("cancelled backup = %s %q by %q; want cancelled %q by the system", b.Status, b.ErrorMessage, b.CancelledBy, want)
	}
	if seen.Reason != reason || seen.By != runs.SystemActor {
		t.Fatalf("the tracked run saw %+v; want the force quit as its cancellation", seen)
	}
	r, err := a.metaStore.GetRestoreRecord(ctx, restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := reason + " (restore cancelled: application force quit)"; r.Status != models.RestoreStatusCancelled || r.ErrorMessage != want ||
		r.CancelledBy != runs.SystemActor {
		t.Fatalf("cancelled restore = %s %q; want cancelled %q", r.Status, r.ErrorMessage, want)
	}
	if f, _ := a.metaStore.GetBackupRecord(ctx, failing.ID); f.Status != models.StatusFailed || f.ErrorMessage != "backup: mongodump failed: exit status 1" || f.CancelledBy != "" {
		t.Fatalf("a backup that failed on its own was rewritten: %+v", f)
	}
	if u, _ := a.metaStore.GetRestoreRecord(ctx, untracked.ID); u.Status != models.RestoreStatusFailed || u.ErrorMessage != "restore aborted: context canceled" {
		t.Fatalf("a run ForceStop did not cancel was rewritten: %+v", u)
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
