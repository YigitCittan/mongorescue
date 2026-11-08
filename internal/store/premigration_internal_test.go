package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

func compatBox(t *testing.T) (*secretbox.Box, *compatFixture) {
	t.Helper()
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	return box, &compatFixture{box: box, ageIdentity: "unused", ageRecip: "unused"}
}

func latestSchema(t *testing.T) int {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	return migrations[len(migrations)-1].version
}

func copyNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pre-v") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestPendingMigrationsTakeACopyFirst(t *testing.T) {
	t.Parallel()
	box, f := compatBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	buildCompatFixture(t, path, f, 5)
	before := schemaSnapshot(t, path)
	latest := latestSchema(t)

	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(3))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf("mongorescue.db.pre-v5-to-v%d", latest)
	if got := copyNames(t, dir); len(got) != 1 || got[0] != want {
		t.Fatalf("copies = %v; want [%s]", got, want)
	}
	copyPath := filepath.Join(dir, want)
	if got := schemaSnapshot(t, copyPath); got != before {
		t.Fatalf("the copy differs from the database before the migration:\nbefore:\n%s\ncopy:\n%s", before, got)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(copyPath)
		if statErr != nil || info.Mode().Perm() != dbFileMode {
			t.Fatalf("copy mode = %v, %v; want %v", info, statErr, dbFileMode)
		}
	}
}

// TestRestoringTheCopyRollsBack simulates a rollback: after the upgrade the copy is put
// back in place of the database, which is at the old schema again and opens.
func TestRestoringTheCopyRollsBack(t *testing.T) {
	t.Parallel()
	box, f := compatBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	buildCompatFixture(t, path, f, 7)
	latest := latestSchema(t)

	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(3))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	// The documented rollback: stop, replace mongorescue.db with the copy, drop the
	// stale -wal/-shm files.
	copyPath := PreMigrationCopyPath(path, 7, latest)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err = os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err = os.Rename(copyPath, path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if version != 7 {
		t.Fatalf("restored schema version = %d; want 7", version)
	}
	// The restored database upgrades again, with the secret key it always had.
	s, err = OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(3))
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.verifySecretsSealed(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestNoPlaintextSecretInAnyPreMigrationCopy upgrades a fixture of every older schema
// version and checks that no pre-migration copy holds a secret in plaintext: a database
// from before the sealing of credentials is not copied at all, a sealed one is.
func TestNoPlaintextSecretInAnyPreMigrationCopy(t *testing.T) {
	t.Parallel()
	latest := latestSchema(t)
	_, f := compatBox(t)
	fixtures := buildCompatFixtures(t, t.TempDir(), f, latest-1)
	secrets := []string{"legacy-pass-1", compatHookSecret, "path-token-5", "header-token-6"}
	for version := 1; version < latest; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(fixtures[version])
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "mongorescue.db")
			if err = os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			// The fixture's sealed values need the key it was built with.
			s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(f.box), WithPreMigrationCopies(3))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			copies := copyNames(t, dir)
			if version == 1 && len(copies) != 0 {
				t.Fatalf("a database from before sealing was copied: %v", copies)
			}
			if version > 1 && len(copies) != 1 {
				t.Fatalf("copies = %v; want one", copies)
			}
			for _, name := range copies {
				data, readErr := os.ReadFile(filepath.Join(dir, name))
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, secret := range secrets {
					if strings.Contains(string(data), secret) {
						t.Errorf("%s holds %q in plaintext", name, secret)
					}
				}
			}
		})
	}
}

func TestNoCopyOfAFreshOrCurrentDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	for range 2 { // create, then reopen at the current schema
		s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithPreMigrationCopies(3))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if got := copyNames(t, dir); len(got) != 0 {
			t.Fatalf("copies = %v; want none", got)
		}
	}
}

func TestCopiesAreOffWhenConfiguredSo(t *testing.T) {
	t.Parallel()
	box, f := compatBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	buildCompatFixture(t, path, f, 5)
	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(0))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if got := copyNames(t, dir); len(got) != 0 {
		t.Fatalf("copies = %v; want none", got)
	}
}

func TestPreMigrationCopiesKeepTheNewest(t *testing.T) {
	t.Parallel()
	box, f := compatBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	buildCompatFixture(t, path, f, 5)
	latest := latestSchema(t)
	old := time.Now().Add(-time.Hour)
	// Copies of earlier upgrades; the oldest ones must go.
	for _, n := range []string{"1-to-v2", "2-to-v3", "3-to-v4"} {
		p := filepath.Join(dir, "mongorescue.db.pre-v"+n)
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated files stay.
	other := filepath.Join(dir, "mongorescue.db.pre-vfoo")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(3))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	want := []string{
		"mongorescue.db.pre-v2-to-v3", "mongorescue.db.pre-v3-to-v4",
		fmt.Sprintf("mongorescue.db.pre-v5-to-v%d", latest),
		"mongorescue.db.pre-vfoo",
	}
	if got := copyNames(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("copies = %v; want %v", got, want)
	}
}

func TestFailedCopyStopsTheMigration(t *testing.T) {
	t.Parallel()
	box, f := compatBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mongorescue.db")
	buildCompatFixture(t, path, f, 5)
	// A non-empty directory where the partial copy goes cannot be cleared.
	blocker := PreMigrationCopyPath(path, 5, latestSchema(t)) + preMigrationTmpSuffix
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box), WithPreMigrationCopies(3))
	if !errors.Is(err, ErrPreMigrationCopy) {
		t.Fatalf("OpenSQLite = %v; want ErrPreMigrationCopy", err)
	}
	// Nothing was migrated.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err = db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("schema version = %d after a failed copy; want 5", version)
	}
}
