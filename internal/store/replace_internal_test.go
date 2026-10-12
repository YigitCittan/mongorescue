package store

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// writeDB creates a database at path holding backup record id.
func writeDB(t *testing.T, box *secretbox.Box, path, id string) {
	t.Helper()
	s, err := OpenSQLite(context.Background(), path, nil, WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// recordOf returns the ID of the one backup record of the database at path.
func recordOf(t *testing.T, box *secretbox.Box, path string) string {
	t.Helper()
	s, err := OpenSQLite(context.Background(), path, nil, WithSecretBox(box))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer s.Close()
	recs, err := s.ListBackupRecords(context.Background(), "")
	if err != nil || len(recs) != 1 {
		t.Fatalf("records of %s = %v, %v", path, recs, err)
	}
	return recs[0].ID
}

// TestReplaceSurvivesACrashAtEveryStep stops the replacement after each step, as a
// crash would, then settles it like the next start does: before the swap the
// previous database stays in use, after it the new one; the journal and the
// replacement file never linger, and the next replacement works.
func TestReplaceSurvivesACrashAtEveryStep(t *testing.T) {
	for _, tc := range []struct {
		step string
		want string
	}{
		{phaseJournaled, "old"},
		{phaseKept, "old"},
		{phaseSwapping, "new"},
		{phaseSwapped, "new"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			ctx := context.Background()
			box := testBox(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "mongorescue.db")
			writeDB(t, box, path, "old")
			newPath := filepath.Join(dir, ".ha-restore", "mongorescue.db")
			if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
				t.Fatal(err)
			}
			writeDB(t, box, newPath, "new")

			replaceCrash = func(s string) bool { return s == tc.step }
			_, err := ReplaceDatabase(ctx, Replacement{Path: path, NewPath: newPath, Epoch: 2}, WithSecretBox(box))
			replaceCrash = nil
			if !errors.Is(err, errCrashed) {
				t.Fatalf("ReplaceDatabase = %v; want the injected crash", err)
			}
			if _, statErr := os.Stat(path + replaceJournalSuffix); statErr != nil {
				t.Fatalf("no journal after the crash: %v", statErr)
			}

			// The next start.
			if err = RecoverReplace(path, nil); err != nil {
				t.Fatal(err)
			}
			if got := recordOf(t, box, path); got != tc.want {
				t.Fatalf("after recovery the database holds %q; want %q", got, tc.want)
			}
			for _, leftover := range []string{path + replaceJournalSuffix, newPath, newPath + "-wal"} {
				if _, statErr := os.Stat(leftover); !errors.Is(statErr, fs.ErrNotExist) {
					t.Fatalf("%s is left after the recovery", leftover)
				}
			}
			// A replacement after the recovery works.
			again := filepath.Join(dir, ".ha-restore", "again.db")
			writeDB(t, box, again, "again")
			if _, err = ReplaceDatabase(ctx, Replacement{Path: path, NewPath: again, Epoch: 3}, WithSecretBox(box)); err != nil {
				t.Fatal(err)
			}
			if got := recordOf(t, box, path); got != "again" {
				t.Fatalf("after the next replacement the database holds %q", got)
			}
		})
	}
}

// TestRecoverReplaceRefusesAMissingDatabase covers a journal whose database is
// gone: the start is refused instead of creating an empty database.
func TestRecoverReplaceRefusesAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	if err := writeJournal(path+replaceJournalSuffix, replaceJournal{NewPath: filepath.Join(dir, "gone.db"), Backup: path + ".bak-x"}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverReplace(path, nil); !errors.Is(err, ErrInterruptedReplace) {
		t.Fatalf("RecoverReplace with the database missing = %v; want ErrInterruptedReplace", err)
	}
}

// TestReplaceHoldsTheDataDirectoryLock checks that a replacement refuses to run
// while a store holds the data directory.
func TestReplaceHoldsTheDataDirectoryLock(t *testing.T) {
	box := testBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	writeDB(t, box, path, "old")
	lock, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	newPath := filepath.Join(t.TempDir(), "new.db")
	writeDB(t, box, newPath, "new")
	if _, err = ReplaceDatabase(context.Background(), Replacement{Path: path, NewPath: newPath}, WithSecretBox(box)); !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("ReplaceDatabase under a held lock = %v; want ErrDataDirLocked", err)
	}
}

// TestReplaceCheckpointsTheOldDatabase checks that the copy of a database left with
// a write-ahead log (its last writes only in the log) holds those writes.
func TestReplaceCheckpointsTheOldDatabase(t *testing.T) {
	ctx := context.Background()
	box := testBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	writeDB(t, box, path, "old")
	// A second connection leaves writes in the log, as a killed process does.
	s, err := OpenSQLite(ctx, path, nil, WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteBackupRecord(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "logged", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	// Abandon the connection without closing the database file cleanly: copy the
	// files away as they are and continue with the copies.
	copyDir := t.TempDir()
	crashed := filepath.Join(copyDir, "mongorescue.db")
	for _, suffix := range []string{"", "-wal"} {
		if err = copyFile(path+suffix, crashed+suffix); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	newPath := filepath.Join(t.TempDir(), "new.db")
	writeDB(t, box, newPath, "new")
	bak, err := ReplaceDatabase(ctx, Replacement{Path: crashed, NewPath: newPath, Epoch: 2}, WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if got := recordOf(t, box, bak); got != "logged" {
		t.Fatalf("the copy of the previous database holds %q; want the write that was only in its log", got)
	}
}

// TestFailedReplacementLeavesNoJournal covers an I/O error (a full disk while the
// previous database is copied): the previous database stays, and no journal is
// left that a later start would report as an interrupted swap.
func TestFailedReplacementLeavesNoJournal(t *testing.T) {
	box := testBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	writeDB(t, box, path, "old")
	newPath := filepath.Join(dir, ".ha-restore", "mongorescue.db")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDB(t, box, newPath, "new")
	diskFull := errors.New("no space left on device")
	replaceFail = func(s string) error {
		if s == phaseKept {
			return diskFull
		}
		return nil
	}
	_, err := ReplaceDatabase(context.Background(), Replacement{Path: path, NewPath: newPath, Epoch: 2}, WithSecretBox(box))
	replaceFail = nil
	if !errors.Is(err, diskFull) {
		t.Fatalf("ReplaceDatabase = %v; want the disk-full error", err)
	}
	if _, statErr := os.Stat(path + replaceJournalSuffix); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatal("a failed replacement left its journal behind")
	}
	if got := recordOf(t, box, path); got != "old" {
		t.Fatalf("the database holds %q after a failed replacement; want the previous one", got)
	}
}

// TestRecoveryWhenTheReplacementFileIsGone covers a crash before the swap after
// which the replacement's temporary directory was removed: the content of the
// database, not the missing file, says that the swap did not happen.
func TestRecoveryWhenTheReplacementFileIsGone(t *testing.T) {
	box := testBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	writeDB(t, box, path, "old")
	tmp := filepath.Join(dir, ".ha-restore")
	newPath := filepath.Join(tmp, "mongorescue.db")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDB(t, box, newPath, "new")
	replaceCrash = func(s string) bool { return s == phaseKept }
	_, err := ReplaceDatabase(context.Background(), Replacement{Path: path, NewPath: newPath, Epoch: 2}, WithSecretBox(box))
	replaceCrash = nil
	if !errors.Is(err, errCrashed) {
		t.Fatal(err)
	}
	if err = os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	if err = RecoverReplace(path, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "before the swap") || !strings.Contains(logs.String(), "phase=kept") {
		t.Fatalf("recovery reported %q; want an interruption before the swap in phase kept", logs.String())
	}
	if got := recordOf(t, box, path); got != "old" {
		t.Fatalf("the database holds %q; want the previous one", got)
	}
}
