package store

import (
	"context"
	"fmt"
)

// CountVaultCredentials returns how many connections and storage targets read
// their credentials from Vault (credential_source vault), without opening any
// sealed value.
func (s *SQLiteStore) CountVaultCredentials(ctx context.Context) (conns, targets int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM connections WHERE credential_source = 'vault'),
		(SELECT COUNT(*) FROM storage_targets WHERE credential_source = 'vault')`).Scan(&conns, &targets)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count credentials from vault: %w", err)
	}
	return conns, targets, nil
}
