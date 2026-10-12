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
	t.Parallel() // independent: own data directories, HA targets and ports
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
	// The takeover imports under the lease, renewed by the manager's loop as in
	// RunHA: acquire first, then import.
	runManager(t, sup.rt.mgr())
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

	status := app.importNewerArchives(ctx, snapshotAt)
	def, err := app.targets.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := status[def.ID]; !ok || st != nil {
		t.Fatalf("import status of the default target = %v (scanned %v); want verified", st, ok)
	}

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

// runManager runs m's lease loop until the test ends and waits until it holds the
// lease.
func runManager(t *testing.T, m *ha.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(30 * time.Second)
	for m.Check() != nil {
		if time.Now().After(deadline) {
			t.Fatal("did not acquire the lease")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
