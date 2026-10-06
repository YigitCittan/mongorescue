//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// copyTargets resolves the fixed storage targets of the copy test.
type copyTargets map[string]*models.StorageTarget

func (c copyTargets) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id == "" {
		id = "tgt_minio"
	}
	if t, ok := c[id]; ok {
		return t, nil
	}
	return nil, errors.New("targets: storage target not found")
}

func (c copyTargets) List(context.Context) ([]*models.StorageTarget, error) {
	out := make([]*models.StorageTarget, 0, len(c))
	for _, t := range c {
		out = append(out, t)
	}
	return out, nil
}

// TestCopyToALocalTargetAndRestoreFallsBack backs a database up to MinIO with a
// copy on a local target, lets the copy queue copy it, damages the primary archive
// in MinIO and restores: the restore reads the copy instead, says so in its record,
// and the clone holds every document.
func TestCopyToALocalTargetAndRestoreFallsBack(t *testing.T) {
	env := requireMongo(t)
	var minio *s3Provider
	for _, p := range configuredS3Providers() {
		if p.Name == "minio" {
			minio = &p
		}
	}
	if minio == nil {
		t.Skip("set the MinIO provider to run the copy test")
	}
	minio.Config.Prefix = "it-copies/" + randomHex(t, 6) + "/"
	primary := newS3Storage(t, *minio)
	cleanupPrefix(t, primary, "")
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]storage.Storage{"tgt_minio": primary, "tgt_local": local}
	resolve := func(_ context.Context, id string) (storage.Storage, error) {
		if id == "" {
			id = "tgt_minio"
		}
		if d, ok := drivers[id]; ok {
			return d, nil
		}
		return nil, errors.New("unknown storage target")
	}

	db := env.uniqueDB(t, "copies")
	env.seed(t, db, "orders", 60)
	st := storetest.New(t)
	queue := copies.New(copies.Config{Store: st, Storages: resolve, Logger: discardLogger})
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store:   st,
		Backup:  newBackupEngine(env, nil, backup.WithStorageResolver(resolve), backup.WithCopier(queue.CopyAll)),
		Restore: newRestoreEngine(env, nil, restore.WithStorageResolver(resolve)),
		Runs:    manager,
		Targets: copyTargets{
			"tgt_minio": {ID: "tgt_minio", Name: "minio", Type: models.StorageS3, IsDefault: true},
			"tgt_local": {ID: "tgt_local", Name: "local copy", Type: models.StorageLocal},
		},
		Storage:     resolve,
		Connections: staticConnections{conn: conn},
		Logger:      discardLogger,
	})
	admin := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())

	started, err := svc.StartBackup(admin, operations.BackupRequest{BackupOptions: models.BackupOptions{
		ConnectionID: conn.ID, Database: db, StorageTargetID: "tgt_minio", CopyTargets: []string{"tgt_local"},
	}})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	bkp := waitBackup(t, svc, started.ID)
	if bkp.Status != models.StatusCompleted || len(bkp.Copies) != 1 || bkp.Copies[0].Status != models.CopyPending {
		t.Fatalf("backup = %s (%s), copies %+v", bkp.Status, bkp.ErrorMessage, bkp.Copies)
	}
	waitIdle(t, manager)
	if err = queue.RunDue(context.Background()); err != nil {
		t.Fatalf("copy queue: %v", err)
	}
	bkp = waitBackup(t, svc, started.ID)
	if c := bkp.Copies[0]; c.Status != models.CopyDone || !c.SHA256OK {
		t.Fatalf("copy = %+v", c)
	}
	if obj, statErr := local.Stat(context.Background(), bkp.StorageKey); statErr != nil || obj.SizeBytes != bkp.SizeBytes {
		t.Fatalf("local copy = %+v, %v; want %d bytes", obj, statErr, bkp.SizeBytes)
	}

	// The primary archive in MinIO is damaged (truncated to a few bytes).
	if _, err = primary.Save(context.Background(), bkp.StorageKey, strings.NewReader("damaged")); err != nil {
		t.Fatal(err)
	}
	rst, err := svc.StartRestore(admin, models.RestoreRequest{BackupID: started.ID})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	done := waitRestore(t, svc, rst.ID)
	if done.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore = %s (%s)", done.Status, done.ErrorMessage)
	}
	if done.SourceTargetID != "tgt_local" || !strings.Contains(done.SourceFallback, "local copy") {
		t.Fatalf("restore read %q, fallback %q; want the local copy", done.SourceTargetID, done.SourceFallback)
	}
	if got := env.count(t, done.TargetDatabase, "orders"); got != 60 {
		t.Fatalf("the clone %s holds %d orders; want 60", done.TargetDatabase, got)
	}
	waitIdle(t, manager)
}
