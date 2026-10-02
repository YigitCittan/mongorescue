package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Tables of the integrity history, as reported by CorruptRecords.
const (
	tableRestoreTests = "restore_tests"
	tableRetentionLog = "retention_log"
)

// Retention of the integrity history tables.
const (
	// MaxRestoreTestsPerJob is how many restore test results are kept per job.
	MaxRestoreTestsPerJob = 200
	// MaxRetentionLogEntries is how many retention log entries are kept in total.
	MaxRetentionLogEntries = 10000
)

// putManifest upserts the manifest of backup id.
func putManifest(ctx context.Context, e execer, id string, m *models.Manifest) error {
	m.Normalize()
	data, err := encode(m)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, `INSERT INTO backup_manifests (backup_id, captured_at, data) VALUES (?, ?, ?)
		ON CONFLICT (backup_id) DO UPDATE SET captured_at = excluded.captured_at, data = excluded.data`,
		id, timeKey(m.CapturedAt), data); err != nil {
		return fmt.Errorf("store: save manifest of %s: %w", id, err)
	}
	return nil
}

// GetManifest returns the manifest captured for backup id, or ErrNotFound when none
// was.
func (s *SQLiteStore) GetManifest(ctx context.Context, id string) (*models.Manifest, error) {
	return getRecord[models.Manifest](ctx, s.db, ErrNotFound, "SELECT data FROM backup_manifests WHERE backup_id = ?", id)
}

// UpdateBackupRecord applies fn to the stored backup record id and saves the result,
// in one transaction, so concurrent partial updates (a pin, a verification result,
// a retention decision) never overwrite each other. When fn returns an error nothing
// is written and the error is returned unchanged. It returns ErrNotFound for an
// unknown id, and the updated record otherwise.
func (s *SQLiteStore) UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error) {
	var out *models.BackupRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rec, err := getRecord[models.BackupRecord](ctx, tx, ErrNotFound, "SELECT data FROM backups WHERE id = ?", id)
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
		if rec.ID != id {
			return fmt.Errorf("%w: backup record id must not change", ErrInvalidRecord)
		}
		if rec.Manifest != nil {
			if err := putManifest(ctx, tx, id, rec.Manifest); err != nil {
				return err
			}
		}
		out = rec
		return putBackup(ctx, tx, rec)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ArchiveKeyIndex maps every storage key named by a backup row on storage target
// targetID to the IDs of those rows. It reads the key with SQL instead of decoding
// the records, so rows that cannot be decoded (skipped by ListBackupRecords) still
// own their archives: callers never treat such an archive as an orphan or delete it.
func (s *SQLiteStore) ArchiveKeyIndex(ctx context.Context, targetID string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, coalesce(json_extract(data, '$.storage_key'), '') FROM backups
		WHERE storage_target_id = ? ORDER BY id`, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: list archive keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]string)
	for rows.Next() {
		var id string
		var key sql.NullString
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("store: scan archive key: %w", err)
		}
		if key.Valid && key.String != "" {
			out[key.String] = append(out[key.String], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list archive keys: %w", err)
	}
	return out, nil
}

// ArchiveReferenceIDs returns the IDs of every backup row, readable or not, that
// names key on storage target targetID.
func (s *SQLiteStore) ArchiveReferenceIDs(ctx context.Context, targetID, key string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM backups
		WHERE storage_target_id = ? AND json_extract(data, '$.storage_key') = ? ORDER BY id`, targetID, key)
	if err != nil {
		return nil, fmt.Errorf("store: list archive references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan archive reference: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list archive references: %w", err)
	}
	return ids, nil
}

// ErrPruneRefused is returned by PruneBackupRecord for a backup retention must keep.
var ErrPruneRefused = errors.New("store: retention must keep this backup")

// PruneBackupRecord marks completed backup id pruned, in one transaction that
// re-checks what retention must keep: a pinned backup, a backup that is no longer
// completed, and the newest backup of its job and database (same connection, storage target and
// scheduled trigger) whose verification is ok, so a verification recorded after
// retention planned cannot be overtaken. A refusal wraps ErrPruneRefused; an unknown
// id returns ErrNotFound.
func (s *SQLiteStore) PruneBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error) {
	var out *models.BackupRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rec, err := getRecord[models.BackupRecord](ctx, tx, ErrNotFound, "SELECT data FROM backups WHERE id = ?", id)
		if err != nil {
			return err
		}
		switch {
		case rec.Pinned:
			return fmt.Errorf("%w: backup %s is pinned", ErrPruneRefused, id)
		case rec.Status != models.StatusCompleted:
			return fmt.Errorf("%w: backup %s is %s", ErrPruneRefused, id, rec.Status)
		}
		if rec.Verification == models.VerificationOK && rec.JobID != "" {
			// Strict: an unreadable sibling could be the newest verified backup, so a
			// row that does not decode refuses the prune instead of being skipped.
			siblings, err := listRecordsStrict[models.BackupRecord](ctx, tx,
				"SELECT data FROM backups WHERE job_id = ? AND database_name = ? AND status = ?", rec.JobID, rec.Database, string(models.StatusCompleted))
			if err != nil {
				return fmt.Errorf("%w: the job's other backups cannot all be read: %w", ErrPruneRefused, err)
			}
			newer := false
			for _, o := range siblings {
				if o.ID != rec.ID && o.Verification == models.VerificationOK && o.StartedAt.After(rec.StartedAt) &&
					o.ConnectionID == rec.ConnectionID && o.StorageTargetID == rec.StorageTargetID &&
					o.EffectiveTrigger() == rec.EffectiveTrigger() {
					newer = true
					break
				}
			}
			if !newer {
				return fmt.Errorf("%w: backup %s is the newest verified backup of its job and database", ErrPruneRefused, id)
			}
		}
		rec.Status = models.StatusPruned
		out = rec
		return putBackup(ctx, tx, rec)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateJobRestoreTest stores summary as the job's latest restore test without
// touching its settings or UpdatedAt. It returns ErrNotFound when the job was deleted.
func (s *SQLiteStore) UpdateJobRestoreTest(ctx context.Context, id string, summary *models.RestoreTestSummary) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		job, err := getRecord[models.Job](ctx, tx, ErrNotFound, "SELECT data FROM jobs WHERE id = ?", id)
		if err != nil {
			return err
		}
		job.LastRestoreTest = summary
		data, err := encode(job)
		if err != nil {
			return err
		}
		return execOne(ctx, tx, ErrNotFound, "UPDATE jobs SET data = ? WHERE id = ?", data, id)
	})
}

// SaveRestoreTest creates or replaces a restore test result and keeps the newest
// MaxRestoreTestsPerJob results of its job.
func (s *SQLiteStore) SaveRestoreTest(ctx context.Context, r *models.RestoreTestResult) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("%w: restore test with ID is required", ErrInvalidRecord)
	}
	data, err := encode(r)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_tests (id, job_id, backup_id, started_at, status, data) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET job_id = excluded.job_id, backup_id = excluded.backup_id,
				started_at = excluded.started_at, status = excluded.status, data = excluded.data`,
			r.ID, r.JobID, r.BackupID, timeKey(r.StartedAt), string(r.Status), data); err != nil {
			return fmt.Errorf("store: save restore test %s: %w", r.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM restore_tests WHERE job_id = ? AND id NOT IN (
			SELECT id FROM restore_tests WHERE job_id = ? ORDER BY started_at DESC, id DESC LIMIT ?)`,
			r.JobID, r.JobID, MaxRestoreTestsPerJob); err != nil {
			return fmt.Errorf("store: prune restore tests: %w", err)
		}
		return nil
	})
}

// ListRestoreTests returns up to limit restore tests of job jobID, newest first.
func (s *SQLiteStore) ListRestoreTests(ctx context.Context, jobID string, limit int) ([]*models.RestoreTestResult, error) {
	return listRecords[models.RestoreTestResult](ctx, s, tableRestoreTests, nil,
		"SELECT id, data FROM restore_tests WHERE job_id = ? ORDER BY started_at DESC, id DESC LIMIT ?", jobID, limit)
}

// AppendRetentionLog stores e, assigns its ID and keeps the newest
// MaxRetentionLogEntries entries.
func (s *SQLiteStore) AppendRetentionLog(ctx context.Context, e *models.RetentionLogEntry) error {
	if e == nil || e.BackupID == "" {
		return fmt.Errorf("%w: retention log entry needs a backup id", ErrInvalidRecord)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		data, err := encode(e)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO retention_log (at, job_id, backup_id, data) VALUES (?, ?, ?, ?)",
			timeKey(e.Time), e.JobID, e.BackupID, data)
		if err != nil {
			return fmt.Errorf("store: append retention log: %w", err)
		}
		if e.ID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: append retention log: %w", err)
		}
		// The ID lives in the column; the stored JSON is rewritten with it.
		if data, err = encode(e); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE retention_log SET data = ? WHERE id = ?", data, e.ID); err != nil {
			return fmt.Errorf("store: append retention log: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM retention_log WHERE id <= ?", e.ID-MaxRetentionLogEntries); err != nil {
			return fmt.Errorf("store: prune retention log: %w", err)
		}
		return nil
	})
}

// ListRetentionLog returns up to limit retention log entries of job jobID, newest
// first.
func (s *SQLiteStore) ListRetentionLog(ctx context.Context, jobID string, limit int) ([]*models.RetentionLogEntry, error) {
	return listRecords[models.RetentionLogEntry](ctx, s, tableRetentionLog, nil,
		"SELECT CAST(id AS TEXT), data FROM retention_log WHERE job_id = ? ORDER BY id DESC LIMIT ?", jobID, limit)
}

// LoadIntegrityState decodes the integrity state document key into v and reports
// whether it exists.
func (s *SQLiteStore) LoadIntegrityState(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM integrity_state WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: load integrity state %s: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("store: decode integrity state %s: %w", key, err)
	}
	return true, nil
}

// SaveIntegrityState stores v as the integrity state document key.
func (s *SQLiteStore) SaveIntegrityState(ctx context.Context, key string, v any) error {
	data, err := encode(v)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO integrity_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, data); err != nil {
		return fmt.Errorf("store: save integrity state %s: %w", key, err)
	}
	return nil
}
