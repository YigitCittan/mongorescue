package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// TestConstraintCodes checks the classification of driver errors by result code,
// including wrapped ones, and that text alone never classifies an error.
func TestConstraintCodes(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err = db.ExecContext(ctx, `CREATE TABLE t (id TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL UNIQUE, n INTEGER CHECK (n >= 0)) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO t (id, name, n) VALUES ('a', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	_, pkErr := db.ExecContext(ctx, `INSERT INTO t (id, name, n) VALUES ('a', 'y', 1)`)
	_, uniqErr := db.ExecContext(ctx, `INSERT INTO t (id, name, n) VALUES ('b', 'x', 1)`)
	_, checkErr := db.ExecContext(ctx, `INSERT INTO t (id, name, n) VALUES ('c', 'z', -1)`)

	cases := []struct {
		name                          string
		err                           error
		primaryKey, uniqueIdx, either bool
	}{
		{"primary key", pkErr, true, false, true},
		{"unique", uniqErr, false, true, true},
		{"wrapped primary key", fmt.Errorf("insert: %w", pkErr), true, false, true},
		{"check constraint", checkErr, false, false, false},
		{"nil", nil, false, false, false},
		{"text only", errors.New("UNIQUE constraint failed: t.id"), false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPrimaryKeyViolation(c.err); got != c.primaryKey {
				t.Errorf("isPrimaryKeyViolation = %v; want %v", got, c.primaryKey)
			}
			if got := isUniqueIndexViolation(c.err); got != c.uniqueIdx {
				t.Errorf("isUniqueIndexViolation = %v; want %v", got, c.uniqueIdx)
			}
			if got := isUniqueViolation(c.err); got != c.either {
				t.Errorf("isUniqueViolation = %v; want %v", got, c.either)
			}
		})
	}
}
