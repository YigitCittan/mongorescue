package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// retiredInstallsSetting lists the metadata backup install IDs that secret key
// rotations retired (JSON list of RetiredInstall). It holds no secret.
const retiredInstallsSetting = "metadata_retired_installs"

// RetiredInstall is a metadata backup install ID that a secret key rotation retired:
// the snapshots below its prefix are sealed with the old key.
type RetiredInstall struct {
	// InstallID is the retired install ID.
	InstallID string `json:"install_id"`
	// RetiredAt is when the rotation committed.
	RetiredAt time.Time `json:"retired_at"`
	// PrunedAt is when its snapshots were deleted (nil until then).
	PrunedAt *time.Time `json:"pruned_at,omitempty"`
}

// readRetiredInstalls returns the retired install IDs.
func readRetiredInstalls(ctx context.Context, q queryer) ([]RetiredInstall, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", retiredInstallsSetting).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read retired install IDs: %w", err)
	}
	var out []RetiredInstall
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%w: retired install IDs: %w", ErrCorruptRecord, err)
	}
	return out, nil
}

// writeRetiredInstalls stores list.
func writeRetiredInstalls(ctx context.Context, e execer, list []RetiredInstall) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if _, err = e.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, retiredInstallsSetting, string(raw)); err != nil {
		return fmt.Errorf("store: save retired install IDs: %w", err)
	}
	return nil
}

// RetiredInstalls returns the metadata backup install IDs that secret key rotations
// retired, oldest first.
func (s *SQLiteStore) RetiredInstalls(ctx context.Context) ([]RetiredInstall, error) {
	return readRetiredInstalls(ctx, s.db)
}

// MarkInstallPruned records that the snapshots of retired install id were deleted.
func (s *SQLiteStore) MarkInstallPruned(ctx context.Context, id string, at time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		list, err := readRetiredInstalls(ctx, tx)
		if err != nil {
			return err
		}
		for i := range list {
			if list[i].InstallID == id && list[i].PrunedAt == nil {
				t := at.UTC()
				list[i].PrunedAt = &t
			}
		}
		return writeRetiredInstalls(ctx, tx, list)
	})
}
