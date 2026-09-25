package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// Compile-time check that SQLiteStore serves the storage targets port.
var _ targets.Repository = (*SQLiteStore)(nil)

const (
	tableStorageTargets = "storage_targets"
	fieldS3SecretKey    = "s3.secret_access_key"

	insertStorageTargetSQL = `INSERT INTO storage_targets (id, name, is_default, data)
		VALUES (?, ?, 0, json_set(?, '$.is_default', json('false')))`

	// updateStorageTargetSQL replaces a target only if it still has the revision
	// (updated_at) the caller read; the default flag is kept from the column.
	updateStorageTargetSQL = `UPDATE storage_targets SET name = ?,
			data = json_set(?, '$.is_default', json(CASE is_default WHEN 1 THEN 'true' ELSE 'false' END))
		WHERE id = ? AND json_extract(data, '$.updated_at') = ?`

	// recordTestSQL stores a test outcome only on the revision that was tested.
	recordTestSQL = `UPDATE storage_targets SET data = json_set(data,
			'$.last_test_at', json(?), '$.last_test_ok', json(?), '$.last_test_error', ?)
		WHERE id = ? AND json_extract(data, '$.updated_at') = ?`
)

// revision renders updated_at exactly as encoding/json stores it in data, which is
// the optimistic concurrency token of storage targets.
func revision(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// liveBackupStatuses are the backup states whose artifact may exist in storage; a
// target they reference cannot be deleted.
var liveBackupStatuses = []any{string(models.StatusCompleted), string(models.StatusInProgress), string(models.StatusPending)}

// ListStorageTargets returns all storage targets sorted by name, secrets decrypted.
func (s *SQLiteStore) ListStorageTargets(ctx context.Context) ([]*models.StorageTarget, error) {
	list, err := listRecords[models.StorageTarget](ctx, s.db, "SELECT data FROM storage_targets ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if err := s.openStorageTarget(t); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// GetStorageTarget returns a storage target or targets.ErrNotFound.
func (s *SQLiteStore) GetStorageTarget(ctx context.Context, id string) (*models.StorageTarget, error) {
	t, err := getRecord[models.StorageTarget](ctx, s.db, targets.ErrNotFound, "SELECT data FROM storage_targets WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if err := s.openStorageTarget(t); err != nil {
		return nil, err
	}
	return t, nil
}

// CreateStorageTarget inserts a new storage target (never the default; see
// SetDefaultStorageTarget), encrypting its S3 secret key. It is the only insert path:
// updates never recreate a deleted target.
func (s *SQLiteStore) CreateStorageTarget(ctx context.Context, t *models.StorageTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("%w: storage target with ID is required", ErrInvalidRecord)
	}
	data, err := s.encodeStorageTarget(t)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, insertStorageTargetSQL, t.ID, t.Name, data); err != nil {
		return fmt.Errorf("store: create storage target %s: %w", t.ID, err)
	}
	return nil
}

// UpdateStorageTarget replaces target t if it still has the revision expected (its
// UpdatedAt when the caller read it); the default flag is kept. It returns
// targets.ErrNotFound for a deleted target and targets.ErrConflict when the target
// changed meanwhile. With locationChanged, it refuses atomically with
// targets.ErrLocationInUse while completed or running backups are stored on it.
func (s *SQLiteStore) UpdateStorageTarget(ctx context.Context, t *models.StorageTarget, expected time.Time, locationChanged bool) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("%w: storage target with ID is required", ErrInvalidRecord)
	}
	data, err := s.encodeStorageTarget(t)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if locationChanged {
			n, countErr := countLiveBackups(ctx, tx, t.ID)
			if countErr != nil {
				return countErr
			}
			if n > 0 {
				return fmt.Errorf("%w: %d backups are stored on it, so its type, path, endpoint, bucket and prefix cannot change; create a new target instead",
					targets.ErrLocationInUse, n)
			}
		}
		res, execErr := tx.ExecContext(ctx, updateStorageTargetSQL, t.Name, data, t.ID, revision(expected))
		if execErr != nil {
			return fmt.Errorf("store: update storage target %s: %w", t.ID, execErr)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		return missingOrChanged(ctx, tx, t.ID)
	})
}

// RecordStorageTargetTest stores the outcome of a test on target id, only if the
// target still has the revision that was tested. It reports whether it was stored;
// a deleted or meanwhile changed target is left alone.
func (s *SQLiteStore) RecordStorageTargetTest(ctx context.Context, id string, tested, at time.Time, ok bool, testErr string) (bool, error) {
	atJSON, err := json.Marshal(at.UTC())
	if err != nil {
		return false, fmt.Errorf("store: encode test time: %w", err)
	}
	res, err := s.db.ExecContext(ctx, recordTestSQL, string(atJSON), strconv.FormatBool(ok), testErr, id, revision(tested))
	if err != nil {
		return false, fmt.Errorf("store: record storage target test %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// missingOrChanged tells a deleted target from a changed one after an update matched
// no row.
func missingOrChanged(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM storage_targets WHERE id = ?", id).Scan(&exists); err != nil {
		return fmt.Errorf("store: check storage target: %w", err)
	}
	if exists == 0 {
		return targets.ErrNotFound
	}
	return targets.ErrConflict
}

// countLiveBackups counts completed or running backups stored on target id.
func countLiveBackups(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	var n int
	args := append([]any{id}, liveBackupStatuses...)
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM backups WHERE storage_target_id = ? AND status IN (?, ?, ?)", args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count backups of storage target: %w", err)
	}
	return n, nil
}

// SetDefaultStorageTarget makes id the only default target in one transaction.
func (s *SQLiteStore) SetDefaultStorageTarget(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM storage_targets WHERE id = ?", id).Scan(&n); err != nil {
			return fmt.Errorf("store: check storage target: %w", err)
		}
		if n == 0 {
			return targets.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE storage_targets SET is_default = 0,
			data = json_set(data, '$.is_default', json('false')) WHERE is_default = 1 AND id != ?`, id); err != nil {
			return fmt.Errorf("store: clear default storage target: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE storage_targets SET is_default = 1,
			data = json_set(data, '$.is_default', json('true')) WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: set default storage target: %w", err)
		}
		return nil
	})
}

// DeleteStorageTarget removes a storage target. In the same transaction it refuses
// with targets.ErrIsDefault for the default target and targets.ErrInUse while a job
// or a completed or running backup references it, so every restorable backup keeps
// its target.
func (s *SQLiteStore) DeleteStorageTarget(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var isDefault int
		err := tx.QueryRowContext(ctx, "SELECT is_default FROM storage_targets WHERE id = ?", id).Scan(&isDefault)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return targets.ErrNotFound
		case err != nil:
			return fmt.Errorf("store: check storage target: %w", err)
		case isDefault == 1:
			return targets.ErrIsDefault
		}
		var jobs int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE storage_target_id = ?", id).Scan(&jobs); err != nil {
			return fmt.Errorf("store: count jobs of storage target: %w", err)
		}
		backups, err := countLiveBackups(ctx, tx, id)
		if err != nil {
			return err
		}
		if jobs > 0 || backups > 0 {
			return fmt.Errorf("%w by %d jobs and %d backups; reassign the jobs and delete those backups first", targets.ErrInUse, jobs, backups)
		}
		return execOne(ctx, tx, targets.ErrNotFound, "DELETE FROM storage_targets WHERE id = ?", id)
	})
}

// AssignStorageTarget records target t on every job and backup record that has no
// storage target yet (rows written before storage targets existed), in one
// transaction. It returns the number of jobs and backups updated.
func (s *SQLiteStore) AssignStorageTarget(ctx context.Context, t *models.StorageTarget) (jobs, backups int, err error) {
	if t == nil || t.ID == "" {
		return 0, 0, fmt.Errorf("%w: storage target with ID is required", ErrInvalidRecord)
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, execErr := tx.ExecContext(ctx, `UPDATE jobs SET storage_target_id = ?,
			data = json_set(data, '$.storage_target_id', ?, '$.storage_type', ?) WHERE storage_target_id = ''`,
			t.ID, t.ID, string(t.Type))
		if execErr != nil {
			return fmt.Errorf("store: assign storage target to jobs: %w", execErr)
		}
		n, _ := res.RowsAffected()
		jobs = int(n)
		res, execErr = tx.ExecContext(ctx, `UPDATE backups SET storage_target_id = ?,
			data = json_set(data, '$.storage_target_id', ?, '$.storage_target_name', ?) WHERE storage_target_id = ''`,
			t.ID, t.ID, t.Name)
		if execErr != nil {
			return fmt.Errorf("store: assign storage target to backups: %w", execErr)
		}
		n, _ = res.RowsAffected()
		backups = int(n)
		return nil
	})
	return jobs, backups, err
}

// encodeStorageTarget seals the S3 secret key of t and returns its JSON document.
func (s *SQLiteStore) encodeStorageTarget(t *models.StorageTarget) (string, error) {
	sealed := t.Clone()
	sealed.UpdatedAt = t.UpdatedAt.UTC()
	if sealed.S3 != nil {
		v, err := s.seal(secretbox.At(tableStorageTargets, t.ID, fieldS3SecretKey), t.S3.SecretAccessKey)
		if err != nil {
			return "", err
		}
		sealed.S3.SecretAccessKey = v
	} else if s.box == nil {
		return "", ErrNoSecretBox
	}
	return encode(sealed)
}

// openStorageTarget decrypts the S3 secret key of t in place.
func (s *SQLiteStore) openStorageTarget(t *models.StorageTarget) error {
	if t.S3 == nil {
		return nil
	}
	v, err := s.open(secretbox.At(tableStorageTargets, t.ID, fieldS3SecretKey), t.S3.SecretAccessKey)
	if err != nil {
		return err
	}
	t.S3.SecretAccessKey = v
	return nil
}
