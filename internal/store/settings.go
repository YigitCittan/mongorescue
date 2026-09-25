package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Compile-time check that SQLiteStore serves the settings port.
var _ settings.Repository = (*SQLiteStore)(nil)

// LoadSettings returns every stored setting and import marker, secret values
// decrypted. Rows the settings package does not know (the key check value) are
// skipped.
func (s *SQLiteStore) LoadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		return nil, fmt.Errorf("store: load settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("store: load settings: %w", err)
		}
		if !settings.IsKnown(key) {
			continue
		}
		if settings.IsSecret(key) {
			if value, err = s.open(secretbox.At(tableSettings, key, fieldSettingValue), value); err != nil {
				return nil, err
			}
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load settings: %w", err)
	}
	return out, nil
}

// SaveSettings upserts values in one transaction, sealing secret values.
func (s *SQLiteStore) SaveSettings(ctx context.Context, values map[string]string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for key, value := range values {
			if !settings.IsKnown(key) {
				return fmt.Errorf("%w: unknown setting %q", ErrInvalidRecord, key)
			}
			if settings.IsSecret(key) {
				var err error
				if value, err = s.seal(secretbox.At(tableSettings, key, fieldSettingValue), value); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
				return fmt.Errorf("store: save setting %s: %w", key, err)
			}
		}
		return nil
	})
}
