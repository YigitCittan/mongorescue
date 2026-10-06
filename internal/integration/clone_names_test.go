//go:build integration

package integration

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestSameSecondSafeClonesBothSucceed starts two restores of one backup in the same
// second (the restore engine's clock is stopped) against a real server, the second
// once the first clone exists, with the preflight on: both complete, each into a
// <db>_rescue_<YYYYMMDD_HHMMSS>_<id> clone of its own holding every document.
func TestSameSecondSafeClonesBothSucceed(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "samesec")
	env.seed(t, db, "orders", 40)

	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := runs.NewManager(discardLogger)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), opTimeout)
		defer scancel()
		_ = manager.Shutdown(sctx)
	})
	at := time.Now().UTC().Truncate(time.Second)
	conn := models.Connection{ID: "conn_it", Name: "integration", URI: env.URI}
	svc := operations.New(operations.Config{
		Store:       storetest.New(t),
		Backup:      newBackupEngine(env, st),
		Restore:     newRestoreEngine(env, st, restore.WithClock(func() time.Time { return at })),
		Runs:        manager,
		Connections: staticConnections{conn: conn},
		Inspector:   mongoconn.New(),
		Logger:      discardLogger,
	})
	admin := auth.WithPrincipal(context.Background(), auth.SystemPrincipal())

	started, err := svc.StartBackup(admin, operations.BackupRequest{BackupOptions: models.BackupOptions{ConnectionID: conn.ID, Database: db}})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if bkp := waitBackup(t, svc, started.ID); bkp.Status != models.StatusCompleted {
		t.Fatalf("backup = %s (%s)", bkp.Status, bkp.ErrorMessage)
	}
	waitIdle(t, manager)

	name := regexp.MustCompile("^" + regexp.QuoteMeta(db+"_rescue_"+at.Format("20060102_150405")+"_") + "[0-9a-f]{4}$")
	var clones []string
	for i := range 2 {
		rst, startErr := svc.StartRestore(admin, models.RestoreRequest{BackupID: started.ID})
		if startErr != nil {
			t.Fatalf("restore %d in the same second: %v", i+1, startErr)
		}
		done := waitRestore(t, svc, rst.ID)
		if done.Status != models.RestoreStatusCompleted || done.Preflight == nil || !done.Preflight.OK {
			t.Fatalf("restore %d = %s (%s), preflight %+v", i+1, done.Status, done.ErrorMessage, done.Preflight)
		}
		if !name.MatchString(done.TargetDatabase) {
			t.Fatalf("restore %d into %q; want %s", i+1, done.TargetDatabase, name)
		}
		if got := env.count(t, done.TargetDatabase, "orders"); got != 40 {
			t.Fatalf("restore %d: %s holds %d orders; want 40", i+1, done.TargetDatabase, got)
		}
		clones = append(clones, done.TargetDatabase)
	}
	waitIdle(t, manager)
	if clones[0] == clones[1] {
		t.Fatalf("both restores went into %s", clones[0])
	}
}
