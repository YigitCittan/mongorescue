package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

func TestMigrationChecksumsOnFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertChecksumsRecorded(t, s)
}

func TestMigrationChecksumsBackfilledOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	f := &compatFixture{box: box, ageIdentity: "unused", ageRecip: "unused"}
	// Schema 0013 predates the checksum column.
	buildCompatFixture(t, path, f, 13)

	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box))
	if err != nil {
		t.Fatalf("open a version 13 database: %v", err)
	}
	assertChecksumsRecorded(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening verifies the backfilled checksums and changes nothing.
	before := schemaSnapshot(t, path)
	s, err = OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if after := schemaSnapshot(t, path); after != before {
		t.Fatalf("reopening changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestOpenRefusesChangedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE schema_migrations SET checksum = ? WHERE version = 3", strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, path)

	_, err = OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrMigrationChanged) {
		t.Fatalf("OpenSQLite = %v; want ErrMigrationChanged", err)
	}
	if !strings.Contains(err.Error(), "0003_settings_storage_targets") {
		t.Fatalf("error %q must name the changed migration", err)
	}
	if after := schemaSnapshot(t, path); after != before {
		t.Fatal("a refused open changed the database")
	}
}

func TestOpenRefusesUnknownAppliedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Version 0 is never embedded and is not newer than the latest migration.
	if _, err = db.Exec("INSERT INTO schema_migrations (version, name, applied_at, checksum) VALUES (0, '0000_branch', 'x', 'x')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrMigrationChanged) || !strings.Contains(err.Error(), "0000_branch") {
		t.Fatalf("OpenSQLite = %v; want ErrMigrationChanged naming 0000_branch", err)
	}
}
