// Package storetest provides a real, disposable metadata store for tests of the
// packages that depend on internal/store.
package storetest

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// New opens an SQLiteStore in a fresh temporary directory and closes it when the test
// and its subtests finish. It fails the test if the store cannot be opened.
func New(tb testing.TB) *store.SQLiteStore {
	tb.Helper()
	return Open(tb, filepath.Join(tb.TempDir(), "mongorescue.db"))
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
