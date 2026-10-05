//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestBackupNowWithCollectionFiltersPerDatabase backs up two real databases in one
// run, each with its own collection filter (one excludes two collections, the other
// includes two), and checks what each archive holds and that each manifest covers
// only the collections backed up.
func TestBackupNowWithCollectionFiltersPerDatabase(t *testing.T) {
	env := requireMongo(t)
	base := env.uniqueDB(t, "filt")
	first, second := base+"_a", base+"_b"
	for _, db := range []string{first, second} {
		for i, coll := range []string{"orders", "customers", "logs", "tmp"} {
			env.seed(t, db, coll, 3+i)
		}
	}

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
	engine := newBackupEngine(env, st, backup.WithManifestCapturer(mongoconn.New().Manifest))
	sched := scheduler.NewScheduler(meta, engine, st, discardLogger, scheduler.WithRunRegistry(registry))
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store: meta, Backup: engine, Restore: newRestoreEngine(env, st), Jobs: sched, Runs: manager,
		Registry: registry, Connections: staticConnections{conn: conn}, Logger: discardLogger,
	})
	ctx := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())

	run, err := svc.StartBackups(ctx, operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: conn.ID},
		Databases: []models.DatabaseFilter{
			{Name: first, ExcludeCollections: []string{"logs", "tmp"}},
			{Name: second, Collections: []string{"orders", "logs"}},
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	want := map[string][]string{
		first:  {"customers", "orders"},
		second: {"logs", "orders"},
	}
	for _, b := range run.Backups {
		done := waitBackup(t, svc, b.ID)
		if done.Status != models.StatusCompleted {
			t.Fatalf("backup of %s = %s (%s)", done.Database, done.Status, done.ErrorMessage)
		}
		list, listErr := svc.ListBackupCollections(ctx, done.ID)
		if listErr != nil {
			t.Fatalf("collections of %s: %v", done.Database, listErr)
		}
		if list.Source != operations.CollectionsFromArchive {
			t.Fatalf("collections of %s come from %s (%s); want the archive", done.Database, list.Source, list.Warning)
		}
		var got []string
		for _, c := range list.Collections {
			got = append(got, c.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want[done.Database]) {
			t.Errorf("archive of %s holds %q; want %q", done.Database, got, want[done.Database])
		}
		m, manifestErr := meta.GetManifest(ctx, done.ID)
		if manifestErr != nil || m == nil {
			t.Fatalf("manifest of %s: %v", done.Database, manifestErr)
		}
		var covered []string
		for _, c := range m.Collections {
			covered = append(covered, c.Name)
		}
		if !slices.Equal(covered, want[done.Database]) {
			t.Errorf("manifest of %s covers %q; want %q", done.Database, covered, want[done.Database])
		}
	}
	waitIdle(t, manager)
}
