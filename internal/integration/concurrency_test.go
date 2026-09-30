//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// staticConnections serves one connection to the operations service.
type staticConnections struct{ conn models.Connection }

func (s staticConnections) Resolve(_ context.Context, id string) (*models.Connection, error) {
	if id != s.conn.ID {
		return nil, connections.ErrNotFound
	}
	c := s.conn
	return &c, nil
}

func (s staticConnections) Get(ctx context.Context, id string) (*models.Connection, error) {
	c, err := s.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	c.URI = "mongodb://******@redacted"
	return c, nil
}

func (s staticConnections) List(ctx context.Context) ([]*models.Connection, error) {
	c, _ := s.Get(ctx, s.conn.ID)
	return []*models.Connection{c}, nil
}

// TestConcurrentRuns drives the operations service shared by the REST API and MCP:
// a second backup of a database that is being backed up is refused with ErrBusy
// (409), a backup and a restore of the same database run side by side (they hold
// different keys), and two restores into the same target are refused like backups.
func TestConcurrentRuns(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "conc")
	env.seedBulk(t, db, 40)
	source := env.snapshotDB(t, db)

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
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store:       meta,
		Backup:      newBackupEngine(env, st),
		Restore:     newRestoreEngine(env, st),
		Runs:        manager,
		Connections: staticConnections{conn: conn},
		Logger:      discardLogger,
	})
	ctx := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())
	backupReq := operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: conn.ID, Database: db}}

	first, err := svc.StartBackup(ctx, backupReq)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if _, err = svc.StartBackup(ctx, backupReq); !errors.Is(err, operations.ErrBusy) {
		t.Fatalf("second backup of the same database: want ErrBusy, got %v", err)
	}
	done := waitBackup(t, svc, first.ID)
	if done.Status != models.StatusCompleted {
		t.Fatalf("first backup: %s (%s)", done.Status, done.ErrorMessage)
	}

	// A restore of the database and a new backup of it at the same time.
	rst, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: done.ID})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	next, err := svc.StartBackup(ctx, backupReq)
	if err != nil {
		t.Fatalf("a backup while a restore of the same database runs must start: %v", err)
	}
	if b := waitBackup(t, svc, next.ID); b.Status != models.StatusCompleted {
		t.Fatalf("backup during restore: %s (%s)", b.Status, b.ErrorMessage)
	}
	r := waitRestore(t, svc, rst.ID)
	if r.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore during backup: %s (%s)", r.Status, r.ErrorMessage)
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, r.TargetDatabase))
	env.dropDB(t, r.TargetDatabase)

	// Two restores into the same target: the second is refused.
	no := false
	inPlace := models.RestoreRequest{BackupID: done.ID, SafeClone: &no, ConfirmInPlace: true, DropTarget: true, TargetDatabase: db + "_copy"}
	a, err := svc.StartRestore(ctx, inPlace)
	if err != nil {
		t.Fatalf("in-place restore: %v", err)
	}
	if _, err = svc.StartRestore(ctx, inPlace); !errors.Is(err, operations.ErrBusy) {
		t.Fatalf("second restore into the same target: want ErrBusy, got %v", err)
	}
	if r := waitRestore(t, svc, a.ID); r.Status != models.RestoreStatusCompleted {
		t.Fatalf("in-place restore: %s (%s)", r.Status, r.ErrorMessage)
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db+"_copy"))
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))
}

func waitBackup(t *testing.T, svc *operations.Service, id string) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(opTimeout)
	for time.Now().Before(deadline) {
		rec, err := svc.GetBackup(context.Background(), id)
		if err != nil {
			t.Fatalf("get backup %s: %v", id, err)
		}
		if rec.Status != models.StatusInProgress {
			return rec
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("backup %s did not finish", id)
	return nil
}

func waitRestore(t *testing.T, svc *operations.Service, id string) *models.RestoreRecord {
	t.Helper()
	deadline := time.Now().Add(opTimeout)
	for time.Now().Before(deadline) {
		rec, err := svc.GetRestore(context.Background(), id)
		if err != nil {
			t.Fatalf("get restore %s: %v", id, err)
		}
		if rec.Status != models.RestoreStatusInProgress {
			return rec
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("restore %s did not finish", id)
	return nil
}
