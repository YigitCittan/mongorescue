//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestSoftDeleteThenPurgeOnRealStorage deletes a backup through the operations
// service and checks on local disk and every configured S3 provider (MinIO,
// LocalStack) that the object survives the deletion, the deletion can be undone, and
// only the purge after the grace period removes it.
func TestSoftDeleteThenPurgeOnRealStorage(t *testing.T) {
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			runSoftDeleteThenPurge(t, target.Storage)
		})
	}
}

func runSoftDeleteThenPurge(t *testing.T, st storage.Storage) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	prefix := "delete-protection-" + randomHex(t, 6) + "/"
	cleanupPrefix(t, st, prefix)
	meta := storetest.New(t)
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{
		Store: meta, Backup: backup.NewEngine(st, ""), Restore: restore.NewEngine(st, ""), Runs: manager,
		Storage: func(context.Context, string) (storage.Storage, error) { return st, nil },
	})
	admin := auth.WithPrincipal(ctx, &auth.Principal{Method: auth.MethodAPIKey, APIKeyName: "stolen", Scope: auth.ScopeAdmin})

	keys := map[string]string{"bkp_one": prefix + "shop/bkp_one.archive.gz", "bkp_two": prefix + "shop/bkp_two.archive.gz"}
	for id, key := range keys {
		if _, err := st.Save(ctx, key, strings.NewReader("archive of "+id)); err != nil {
			t.Fatalf("save %s: %v", key, err)
		}
		if err := meta.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted,
			StorageKey: key, StartedAt: time.Now().UTC().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}

	// A single and a bulk delete leave both objects in place.
	res, err := svc.DeleteBackup(admin, "bkp_one", "integration")
	if err != nil || res.ArchiveDeleted {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	bulk, err := svc.Bulk(admin, operations.BulkBackups, operations.BulkRequest{Action: operations.BulkDelete, IDs: []string{"bkp_two"}})
	if err != nil || bulk.Succeeded != 1 {
		t.Fatalf("bulk delete = %+v, %v", bulk, err)
	}
	for _, key := range keys {
		if _, err = st.Stat(ctx, key); err != nil {
			t.Fatalf("object %s gone right after the delete: %v", key, err)
		}
	}

	// Undo brings bkp_one back; it is no longer due for the purge.
	if _, err = svc.UndeleteBackup(admin, "bkp_one"); err != nil {
		t.Fatal(err)
	}

	grace := models.GraceDuration(models.DefaultDeleteGraceDays)
	storages := func(context.Context, string) (storage.Storage, error) { return st, nil }
	early, err := scheduler.PurgeDeleted(ctx, time.Now().Add(grace-time.Hour), grace, meta, storages, discardLogger, nil)
	if err != nil || len(early) != 0 {
		t.Fatalf("purge before the grace period = %v, %v", early, err)
	}
	purged, err := scheduler.PurgeDeleted(ctx, time.Now().Add(grace+time.Hour), grace, meta, storages, discardLogger, nil)
	if err != nil || len(purged) != 1 || purged[0] != "bkp_two" {
		t.Fatalf("purge after the grace period = %v, %v; want bkp_two", purged, err)
	}
	if _, err = st.Stat(ctx, keys["bkp_two"]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("purged object still there: %v", err)
	}
	if _, err = st.Stat(ctx, keys["bkp_one"]); err != nil {
		t.Fatalf("undeleted backup's object removed: %v", err)
	}
	rec, err := meta.GetBackupRecord(ctx, "bkp_two")
	if err != nil || rec.Status != models.StatusPurged {
		t.Fatalf("record = %+v, %v; want purged", rec, err)
	}
}
