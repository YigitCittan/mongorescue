//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestBackupNowOfThreeDatabases backs up three real databases in one on-demand run
// (POST /api/v1/backups with databases): each into its own archive under the same
// run ID, two at a time, with users and roles; one of them is restored into a safe
// clone and compared with its source.
func TestBackupNowOfThreeDatabases(t *testing.T) {
	env := requireMongo(t)
	base := env.uniqueDB(t, "now")
	dbs := []string{base + "_a", base + "_b", base + "_c"}
	for i, db := range dbs {
		env.seed(t, db, "items", 5+i)
	}
	source := env.snapshotDB(t, dbs[1])

	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta := storetest.New(t)
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	registry := runs.NewRegistry()
	engine := newBackupEngine(env, st)
	sched := scheduler.NewScheduler(meta, engine, st, discardLogger, scheduler.WithRunRegistry(registry))
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store: meta, Backup: engine, Restore: newRestoreEngine(env, st), Jobs: sched, Runs: manager,
		Registry: registry, Connections: staticConnections{conn: conn}, Logger: discardLogger,
	})
	ctx := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())

	two := 2
	run, err := svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: conn.ID, IncludeUsersAndRoles: true},
		Databases:     models.DatabaseNames(dbs...), Parallelism: &two,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(run.Backups) != 3 || len(run.Busy) != 0 {
		t.Fatalf("run = %+v; want three backups", run)
	}
	var restorable *models.BackupRecord
	for i, b := range run.Backups {
		done := waitBackup(t, svc, b.ID)
		if done.Status != models.StatusCompleted || done.Database != dbs[i] || done.RunID != run.RunID || done.SizeBytes <= 0 || !done.UsersAndRoles {
			t.Fatalf("backup of %s = %+v", dbs[i], done)
		}
		if done.Database == dbs[1] {
			restorable = done
		}
	}
	waitIdle(t, manager)
	page, err := meta.QueryBackupRecords(ctx, store.BackupFilter{RunID: run.RunID})
	if err != nil || page.Total != 3 {
		t.Fatalf("backups of run %s = %d, %v", run.RunID, page.Total, err)
	}
	// Nothing is left tracked once the run ended.
	for _, b := range run.Backups {
		deadline := time.Now().Add(opTimeout)
		for registry.Get(b.ID) != nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if registry.Get(b.ID) != nil {
			t.Errorf("backup %s is still tracked after the run", b.ID)
		}
	}

	rst, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: restorable.ID})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	r := waitRestore(t, svc, rst.ID)
	if r.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore: %s (%s)", r.Status, r.ErrorMessage)
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, r.TargetDatabase))
	env.dropDB(t, r.TargetDatabase)
}
