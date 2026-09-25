package store

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestAPIKeysCreatedBeforeScopesBecomeAdmin applies the migrations up to 0003, stores
// a key the way earlier releases did (without a scope) and checks that opening the
// database with this build migrates it to the admin scope it effectively had.
func TestAPIKeysCreatedBeforeScopesBecomeAdmin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 3 {
			break
		}
		if _, err = db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?, ?, ?)", m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO api_keys (id, name, prefix, key_hash, created_by, created_at) VALUES ('key_old', 'old', 'abcdefgh', 'hash', '', 1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	k, err := s.GetAPIKeyByPrefix(ctx, "abcdefgh")
	if err != nil || k.Scope != auth.ScopeAdmin {
		t.Fatalf("migrated key = %+v, %v; want the admin scope", k, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET scope = 'root' WHERE id = 'key_old'`); err == nil {
		t.Fatal("the scope column must only accept read, operator and admin")
	}
}
