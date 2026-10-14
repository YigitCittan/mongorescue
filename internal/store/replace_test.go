package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// snapshotOf writes a database holding one backup record with id to a snapshot file.
func snapshotOf(t *testing.T, box *secretbox.Box, id string) string {
	t.Helper()
	ctx := context.Background()
	src, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "source.db"), nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err = src.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err = src.VacuumInto(ctx, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// liveDatabase creates the database at path with one backup record with id.
func liveDatabase(t *testing.T, box *secretbox.Box, path, id string) {
	t.Helper()
	ctx := context.Background()
	live, err := store.OpenSQLite(ctx, path, nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if err = live.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err = live.Close(); err != nil {
		t.Fatal(err)
	}
}

// hasRecord reports whether the database at path holds backup record id.
func hasRecord(t *testing.T, box *secretbox.Box, path, id string) bool {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), path, nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.GetBackupRecord(context.Background(), id)
	return err == nil
}

func TestReplaceDatabaseKeepsTheOldOneAsBackup(t *testing.T) {
	ctx := context.Background()
	box := storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	liveDatabase(t, box, path, "old")

	snap := snapshotOf(t, box, "new")
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	bak, err := store.ReplaceDatabase(ctx, store.Replacement{Path: path, NewPath: snap, Epoch: 7, Now: func() time.Time { return at }},
		store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if want := path + ".bak-20261012T100000Z-epoch7"; bak != want {
		t.Fatalf("backup %s; want %s", bak, want)
	}
	if _, err = os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the snapshot file was not moved into place")
	}
	if !hasRecord(t, box, path, "new") || hasRecord(t, box, path, "old") {
		t.Fatal("the replaced database does not hold exactly the snapshot's records")
	}
	if !hasRecord(t, box, bak, "old") {
		t.Fatal("the backup lacks the old record")
	}
}

// TestReplaceDatabaseKeepsTheNewestBackups replaces the database four times: the
// three newest copies of previous databases are kept, never fewer.
func TestReplaceDatabaseKeepsTheNewestBackups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	box := storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	liveDatabase(t, box, path, "gen0")
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		id := "gen" + string(rune('0'+i))
		at = at.Add(time.Minute)
		now := at
		if _, err := store.ReplaceDatabase(ctx, store.Replacement{Path: path, NewPath: snapshotOf(t, box, id), Epoch: uint64(i),
			Now: func() time.Time { return now }}, store.WithSecretBox(box)); err != nil {
			t.Fatal(err)
		}
	}
	backups, err := store.DatabaseBackups(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != store.DefaultBackupsKept {
		t.Fatalf("%d backups kept: %v; want %d", len(backups), backups, store.DefaultBackupsKept)
	}
	// The newest backup holds the database replaced last (gen3), the oldest kept gen1.
	if !hasRecord(t, box, backups[len(backups)-1], "gen3") || !hasRecord(t, box, backups[0], "gen1") {
		t.Fatalf("the kept backups are not the newest: %v", backups)
	}
	if !strings.HasSuffix(backups[0], "-epoch2") {
		t.Fatalf("oldest kept backup %s; want the one replaced at epoch 2", backups[0])
	}
}

func TestReplaceDatabaseRefusesAnotherKey(t *testing.T) {
	ctx := context.Background()
	mine, theirs := storetest.NewBox(t), storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	liveDatabase(t, mine, path, "mine")
	// A snapshot sealed with another key is refused, and the database is untouched.
	snap := snapshotOf(t, theirs, "theirs")
	if _, err := store.ReplaceDatabase(ctx, store.Replacement{Path: path, NewPath: snap}, store.WithSecretBox(mine)); !errors.Is(err, store.ErrInvalidReplacement) {
		t.Fatalf("ReplaceDatabase with another key = %v; want ErrInvalidReplacement", err)
	}
	// Garbage is refused too.
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceDatabase(ctx, store.Replacement{Path: path, NewPath: junk}, store.WithSecretBox(mine)); !errors.Is(err, store.ErrInvalidReplacement) {
		t.Fatalf("ReplaceDatabase with junk = %v; want ErrInvalidReplacement", err)
	}
	if !hasRecord(t, mine, path, "mine") {
		t.Fatal("the database changed after a refused replacement")
	}
}

func TestReplaceDatabaseWithoutAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	box := storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	bak, err := store.ReplaceDatabase(ctx, store.Replacement{Path: path, NewPath: snapshotOf(t, box, "new")}, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if backups, _ := store.DatabaseBackups(path); bak != "" || len(backups) != 0 {
		t.Fatalf("a backup of a database that did not exist: %q %v", bak, backups)
	}
}
