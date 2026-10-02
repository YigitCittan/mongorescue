package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrSnapshotExists is returned by VacuumInto when the destination already exists:
// SQLite never overwrites it.
var ErrSnapshotExists = errors.New("store: snapshot destination already exists")

// VacuumInto writes a consistent online snapshot of the metadata database to the new
// file path with VACUUM INTO. The database stays usable: the store has a single
// connection, so the snapshot waits for a running transaction and the next one waits
// for the snapshot. Secrets in the copy stay sealed with the store's secret box.
func (s *SQLiteStore) VacuumInto(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return ErrSnapshotExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: snapshot destination: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	return nil
}
