// Package storetest provides a real, disposable metadata store for tests of the
// packages that depend on internal/store.
package storetest

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// New opens an SQLiteStore in a fresh temporary directory and closes it when the test
// and its subtests finish. It fails the test if the store cannot be opened.
//
// The database starts as a copy of one migrated once per test binary (without a
// secret box, so opening it with the test's box seals it as a new database is), so
// a test does not pay for applying every migration again.
func New(tb testing.TB) *store.SQLiteStore {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "mongorescue.db")
	data, err := migratedTemplate()
	if err != nil {
		tb.Fatalf("migrate the template database: %v", err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatalf("copy the template database: %v", err)
	}
	return Open(tb, path)
}

// template is the migrated empty database New copies.
var template struct {
	once sync.Once
	data []byte
	err  error
}

// migratedTemplate returns the bytes of an empty database with every migration
// applied, created on the first call (in a temporary directory removed at once).
func migratedTemplate() ([]byte, error) {
	template.once.Do(func() {
		dir, err := os.MkdirTemp("", "storetest-*")
		if err != nil {
			template.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "mongorescue.db")
		s, err := store.OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
		if err != nil {
			template.err = err
			return
		}
		// Close checkpoints the write-ahead log into the database file.
		if err = s.Close(); err != nil {
			template.err = err
			return
		}
		template.data, template.err = os.ReadFile(path)
	})
	return template.data, template.err
}

// Open opens the SQLiteStore at path with a fresh random secret key and closes it when
// the test finishes.
func Open(tb testing.TB, path string) *store.SQLiteStore {
	tb.Helper()
	return OpenWithBox(tb, path, NewBox(tb))
}

// OpenWithBox opens the SQLiteStore at path encrypting with box (reopening a database
// requires the box it was created with) and closes it when the test finishes.
func OpenWithBox(tb testing.TB, path string, box *secretbox.Box) *store.SQLiteStore {
	tb.Helper()
	s, err := store.OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), store.WithSecretBox(box))
	if err != nil {
		tb.Fatalf("open metadata store: %v", err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Errorf("close metadata store: %v", err)
		}
	})
	return s
}

// NewBox returns a secret box with a random key.
func NewBox(tb testing.TB) *secretbox.Box {
	tb.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		tb.Fatal(err)
	}
	return box
}
