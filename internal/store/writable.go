package store

import (
	"context"
	"database/sql"
	"fmt"
)

// CheckWritable reports whether the database accepts a write: it opens a write
// transaction and runs a statement that changes no row, which fails on a read-only
// or locked database, a failing disk or a closed store. A full disk is the disk
// guard's to report. Only the readiness check calls it.
func (s *SQLiteStore) CheckWritable(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ''"); err != nil {
			return fmt.Errorf("store: write probe: %w", err)
		}
		return nil
	})
}
