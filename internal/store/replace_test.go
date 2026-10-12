package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

func TestReplaceDatabaseKeepsTheOldOneAsBackup(t *testing.T) {
	ctx := context.Background()
	box := storetest.NewBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	live, err := store.OpenSQLite(ctx, path, nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if err = live.SaveBackupRecord(ctx, &models.BackupRecord{ID: "old", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err = live.Close(); err != nil {
		t.Fatal(err)
	}

	snap := snapshotOf(t, box, "new")
	if err = store.ReplaceDatabase(ctx, path, snap, nil, store.WithSecretBox(box)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the snapshot file was not moved into place")
	}
	replaced, err := store.OpenSQLite(ctx, path, nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	defer replaced.Close()
	if _, err = replaced.GetBackupRecord(ctx, "new"); err != nil {
		t.Fatalf("the replaced database lacks the snapshot's record: %v", err)
	}
	if _, err = replaced.GetBackupRecord(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the replaced database still has the old record: %v", err)
	}
	bak, err := store.OpenSQLite(ctx, path+store.BackupSuffix, nil, store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	if _, err = bak.GetBackupRecord(ctx, "old"); err != nil {
		t.Fatalf("the backup lacks the old record: %v", err)
	}
}

func TestReplaceDatabaseRefusesAnotherKey(t *testing.T) {
	ctx := context.Background()
	mine, theirs := storetest.NewBox(t), storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	live, err := store.OpenSQLite(ctx, path, nil, store.WithSecretBox(mine))
	if err != nil {
		t.Fatal(err)
	}
	if err = live.SaveBackupRecord(ctx, &models.BackupRecord{ID: "mine", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	_ = live.Close()
	// A snapshot sealed with another key is refused, and the database is untouched.
	snap := snapshotOf(t, theirs, "theirs")
	if err = store.ReplaceDatabase(ctx, path, snap, nil, store.WithSecretBox(mine)); !errors.Is(err, store.ErrInvalidReplacement) {
		t.Fatalf("ReplaceDatabase with another key = %v; want ErrInvalidReplacement", err)
	}
	// Garbage is refused too.
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err = os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = store.ReplaceDatabase(ctx, path, junk, nil, store.WithSecretBox(mine)); !errors.Is(err, store.ErrInvalidReplacement) {
		t.Fatalf("ReplaceDatabase with junk = %v; want ErrInvalidReplacement", err)
	}
	again, err := store.OpenSQLite(ctx, path, nil, store.WithSecretBox(mine))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err = again.GetBackupRecord(ctx, "mine"); err != nil {
		t.Fatalf("the database changed after a refused replacement: %v", err)
	}
}

func TestReplaceDatabaseWithoutAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	box := storetest.NewBox(t)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	if err := store.ReplaceDatabase(ctx, path, snapshotOf(t, box, "new"), nil, store.WithSecretBox(box)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + store.BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a backup of a database that did not exist: %v", err)
	}
}
