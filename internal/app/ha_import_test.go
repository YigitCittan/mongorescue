package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// TestTakeoverImportsArchivesNewerThanTheSnapshot checks the import of a takeover:
// an orphan archive written after the snapshot the instance took over from is
// imported; an older orphan is left alone, and nothing is deleted.
func TestTakeoverImportsArchivesNewerThanTheSnapshot(t *testing.T) {
	ctx := context.Background()
	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := newTestSupervisor(t, cfg)
	standby, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(ctx, standby); err != nil {
		t.Fatal(err)
	}
	_ = standby.Close()
	sup.rt.mgr().Step(ctx)
	if sup.rt.mgr().Role() != ha.RoleActive {
		t.Fatal("did not acquire the lease")
	}
	app, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	backups, err := cfg.DefaultBackupsDir()
	if err != nil {
		t.Fatal(err)
	}
	write := func(key string, mod time.Time) {
		t.Helper()
		path := filepath.Join(backups, filepath.FromSlash(key))
		if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("archive "+key), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	snapshotAt := time.Now().Add(-time.Hour)
	newer := "shop/2026/10/bkp_shop_20261012_100000_abcdef12.archive"
	older := "shop/2026/09/bkp_shop_20260901_100000_abcdef12.archive"
	write(newer, time.Now())
	write(older, snapshotAt.Add(-24*time.Hour))

	app.importNewerArchives(ctx, snapshotAt)

	page, err := app.metaStore.QueryBackupRecords(ctx, store.BackupFilter{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Record.StorageKey != newer || !page.Rows[0].Record.Imported {
		var keys []string
		for _, r := range page.Rows {
			keys = append(keys, r.Record.StorageKey)
		}
		t.Fatalf("imported %v; want only %s", keys, newer)
	}
	for _, key := range []string{newer, older} {
		if _, statErr := os.Stat(filepath.Join(backups, filepath.FromSlash(key))); statErr != nil {
			t.Fatalf("the takeover removed %s: %v", key, statErr)
		}
	}
	// The import hashes the archive in the background.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rec, getErr := app.metaStore.GetBackupRecord(ctx, page.Rows[0].Record.ID)
		if getErr == nil && rec.Status == models.StatusCompleted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the imported archive never completed")
}
