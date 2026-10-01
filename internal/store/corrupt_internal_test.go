package store

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// loosenTable rebuilds table without STRICT typing, keeping its rows, so a test can
// store a value of the wrong type in a column (as a database edited by hand or by
// another tool could hold).
func loosenTable(t *testing.T, s *SQLiteStore, table string) {
	t.Helper()
	ctx := context.Background()
	var ddl string
	if err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	loose := strings.Replace(strings.TrimSuffix(strings.TrimSpace(ddl), "STRICT"), "CREATE TABLE "+table, "CREATE TABLE "+table+"_loose", 1)
	for _, stmt := range []string{
		"PRAGMA foreign_keys = OFF",
		loose,
		"INSERT INTO " + table + "_loose SELECT * FROM " + table,
		"DROP TABLE " + table,
		"ALTER TABLE " + table + "_loose RENAME TO " + table,
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func TestListsSkipRowsWithColumnsOfTheWrongType(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	for _, id := range []string{"usr_a", "usr_bad"} {
		if err = s.CreateUser(ctx, &auth.User{ID: id, Username: id, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"key_a", "key_bad"} {
		if err = s.CreateAPIKey(ctx, &auth.APIKey{ID: id, Name: id, Prefix: "p_" + id, Hash: "h", CreatedBy: "usr_a", CreatedAt: now, Scope: auth.ScopeRead}); err != nil {
			t.Fatal(err)
		}
	}
	const stored = "yesterday-7731"
	for _, table := range []string{"users", "api_keys"} {
		loosenTable(t, s, table)
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE users SET created_at = ? WHERE id = 'usr_bad'", stored); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE id = 'key_bad'", stored); err != nil {
		t.Fatal(err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers failed as a whole: %v", err)
	}
	if len(users) != 1 || users[0].ID != "usr_a" {
		t.Fatalf("ListUsers = %+v; want usr_a", users)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys failed as a whole: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "key_a" {
		t.Fatalf("ListAPIKeys = %+v; want key_a", keys)
	}

	// Single-row reads fail with ErrCorruptRecord, without quoting the stored value.
	_, getErr := s.GetUser(ctx, "usr_bad")
	if !errors.Is(getErr, ErrCorruptRecord) || strings.Contains(getErr.Error(), stored) {
		t.Fatalf("GetUser(usr_bad) = %v; want ErrCorruptRecord without the stored value", getErr)
	}
	if _, getErr = s.GetAPIKeyByPrefix(ctx, "p_key_bad"); !errors.Is(getErr, ErrCorruptRecord) || strings.Contains(getErr.Error(), stored) {
		t.Fatalf("GetAPIKeyByPrefix(key_bad) = %v; want ErrCorruptRecord without the stored value", getErr)
	}

	bad, err := s.CorruptRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []CorruptRecord{
		{Table: "api_keys", ID: "key_bad", Error: "a column has a value of the wrong type"},
		{Table: "users", ID: "usr_bad", Error: "a column has a value of the wrong type"},
	}
	if len(bad) != len(want) || bad[0] != want[0] || bad[1] != want[1] {
		t.Fatalf("CorruptRecords = %+v; want %+v", bad, want)
	}
}

func TestScanRowError(t *testing.T) {
	conv := errors.New(`sql: Scan error on column index 3, name "created_at": converting driver.Value type string ("yesterday-7731") to a int64: invalid syntax`)
	err := scanRowError("user", conv)
	if !errors.Is(err, ErrCorruptRecord) || !errors.Is(err, conv) || strings.Contains(err.Error(), "yesterday") {
		t.Fatalf("conversion error = %v; want ErrCorruptRecord without the value", err)
	}
	if got := corruptSummary(err); got != "a column has a value of the wrong type" {
		t.Fatalf("summary = %q", got)
	}
	ioErr := errors.New("disk I/O error")
	if err := scanRowError("user", ioErr); errors.Is(err, ErrCorruptRecord) || !errors.Is(err, ioErr) {
		t.Fatalf("I/O error = %v; want a plain error", err)
	}
}
