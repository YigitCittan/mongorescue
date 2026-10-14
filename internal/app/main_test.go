package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/store"
)

// TestMain makes the Apps of the tests cheap to start (see openMetadataStore and
// bcryptCost): under the race detector, migrating a new metadata database and
// hashing at bcrypt's default cost took most of the time of every App, and the HA
// tests start several.
func TestMain(m *testing.M) {
	template, cleanup, err := migratedTemplate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate the template metadata database:", err)
		os.Exit(1)
	}
	bcryptCost = bcrypt.MinCost
	openMetadataStore = func(ctx context.Context, path string, logger *slog.Logger, opts ...store.Option) (*store.SQLiteStore, error) {
		// A new database starts as a copy of the migrated empty one, which the App
		// then seals with its key exactly as it seals a new database.
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			if err := os.WriteFile(path, template, 0o600); err != nil {
				return nil, err
			}
		}
		return store.OpenSQLite(ctx, path, logger, opts...)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// migratedTemplate returns the file of an empty metadata database with every
// migration applied (without a secret box) and a function removing its directory.
func migratedTemplate() ([]byte, func(), error) {
	dir, err := os.MkdirTemp("", "app-template-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "mongorescue.db")
	s, err := store.OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err = s.Close(); err != nil {
		cleanup()
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return data, cleanup, nil
}
