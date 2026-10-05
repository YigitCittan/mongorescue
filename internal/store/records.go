package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

const (
	upsertJobSQL = `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, storage_target_id, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, database_name = excluded.database_name,
			enabled = excluded.enabled, created_at = excluded.created_at,
			connection_id = excluded.connection_id, storage_target_id = excluded.storage_target_id,
			data = excluded.data`

	upsertBackupSQL = `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, retry_of, size_bytes, phases, run_id, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET job_id = excluded.job_id, database_name = excluded.database_name,
			status = excluded.status, started_at = excluded.started_at,
			connection_id = excluded.connection_id, storage_target_id = excluded.storage_target_id,
			retry_of = excluded.retry_of, size_bytes = excluded.size_bytes, phases = excluded.phases,
			run_id = excluded.run_id, data = excluded.data`

	upsertRestoreSQL = `INSERT INTO restores (id, backup_id, source_database, target_database, status, started_at, phases, verification, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET backup_id = excluded.backup_id, source_database = excluded.source_database,
			target_database = excluded.target_database, status = excluded.status,
			started_at = excluded.started_at, phases = excluded.phases, verification = excluded.verification,
			data = excluded.data`
)

// fieldJobHeartbeatURL is the sealed heartbeat URL of a job.
const fieldJobHeartbeatURL = "heartbeat_url"

// sealJob returns job with its heartbeat URL sealed for storage (job itself when it
// has none). A value that is sealed already is kept, so a job read raw from the
// database is never sealed twice.
func (s *SQLiteStore) sealJob(job *models.Job) (*models.Job, error) {
	if job.HeartbeatURL == "" || secretbox.IsSealed(job.HeartbeatURL) {
		return job, nil
	}
	v, err := s.seal(secretbox.At(tableJobs, job.ID, fieldJobHeartbeatURL), job.HeartbeatURL)
	if err != nil {
		return nil, err
	}
	sealed := *job
	sealed.HeartbeatURL = v
	return &sealed, nil
}

// tableJobHeartbeats names the heartbeat URLs of jobs in CorruptRecord.Table: a URL
// that cannot be opened is reported there, apart from the job, which stays usable.
const tableJobHeartbeats = "jobs.heartbeat_url"

// openJob decrypts job.HeartbeatURL in place. A URL that cannot be opened (planted
// plaintext, a damaged or swapped ciphertext) never makes the job unreadable: it is
// dropped, so the job runs without its heartbeat, and reported through
// CorruptRecords (table jobs.heartbeat_url) until the URL is entered again. Only a
// missing secret box is an error.
func (s *SQLiteStore) openJob(job *models.Job) error {
	err := s.openJobHeartbeat(job)
	switch {
	case err == nil:
		s.clearCorrupt(tableJobHeartbeats, job.ID)
		return nil
	case errors.Is(err, ErrNoSecretBox):
		return err
	}
	job.HeartbeatURL = ""
	s.reportCorruptField(tableJobHeartbeats, job.ID, err)
	return nil
}

// openJobHeartbeat decrypts job.HeartbeatURL in place, refusing plaintext (see
// open).
func (s *SQLiteStore) openJobHeartbeat(job *models.Job) error {
	v, err := s.open(secretbox.At(tableJobs, job.ID, fieldJobHeartbeatURL), job.HeartbeatURL)
	if err != nil {
		return err
	}
	job.HeartbeatURL = v
	return nil
}

// SaveJob creates or updates a scheduled backup job. It sets CreatedAt (when zero) and
// UpdatedAt on job before persisting it; its heartbeat URL is sealed.
func (s *SQLiteStore) SaveJob(ctx context.Context, job *models.Job) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("%w: job with ID is required", ErrInvalidRecord)
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	sealed, err := s.sealJob(job)
	if err != nil {
		return err
	}
	return putJob(ctx, s.db, sealed)
}

// CreateJob inserts a new job; it returns ErrAlreadyExists when the ID is taken.
func (s *SQLiteStore) CreateJob(ctx context.Context, job *models.Job) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("%w: job with ID is required", ErrInvalidRecord)
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	sealed, err := s.sealJob(job)
	if err != nil {
		return err
	}
	data, err := encode(sealed)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, storage_target_id, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		job.ID, job.Name, job.Database, boolInt(job.Enabled), timeKey(job.CreatedAt), job.ConnectionID, job.StorageTargetID, data)
	if err != nil {
		return fmt.Errorf("store: create job %s: %w", job.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: job %s", ErrAlreadyExists, job.ID)
	}
	return nil
}

// UpdateJob replaces an existing job; it returns ErrNotFound when the job was
// deleted, so a concurrent delete is never undone.
func (s *SQLiteStore) UpdateJob(ctx context.Context, job *models.Job) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("%w: job with ID is required", ErrInvalidRecord)
	}
	job.UpdatedAt = time.Now().UTC()
	sealed, err := s.sealJob(job)
	if err != nil {
		return err
	}
	data, err := encode(sealed)
	if err != nil {
		return err
	}
	return execOne(ctx, s.db, ErrNotFound, `UPDATE jobs SET name = ?, database_name = ?, enabled = ?, created_at = ?,
		connection_id = ?, storage_target_id = ?, data = ? WHERE id = ?`,
		job.Name, job.Database, boolInt(job.Enabled), timeKey(job.CreatedAt), job.ConnectionID, job.StorageTargetID, data, job.ID)
}

// UpdateJobRunTimes stores a job's run timestamps without touching its settings, so
// a run finishing or a reschedule never reverts an edit saved meanwhile. A nil
// lastRun or nextRun keeps the stored value. UpdatedAt is not changed. It returns
// ErrNotFound when the job was deleted.
func (s *SQLiteStore) UpdateJobRunTimes(ctx context.Context, id string, lastRun, nextRun *time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		job, err := getRecord[models.Job](ctx, tx, ErrNotFound, "SELECT data FROM jobs WHERE id = ?", id)
		if err != nil {
			return err
		}
		if lastRun != nil {
			last := lastRun.UTC()
			job.LastRun = &last
		}
		if nextRun != nil {
			next := nextRun.UTC()
			job.NextRun = &next
		}
		data, err := encode(job)
		if err != nil {
			return err
		}
		return execOne(ctx, tx, ErrNotFound, "UPDATE jobs SET data = ? WHERE id = ?", data, id)
	})
}

// GetJob returns a job by ID (its heartbeat URL decrypted) or ErrNotFound.
func (s *SQLiteStore) GetJob(ctx context.Context, id string) (*models.Job, error) {
	job, err := getRecord[models.Job](ctx, s.db, ErrNotFound, "SELECT data FROM jobs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if err = s.openJob(job); err != nil {
		return nil, err
	}
	return job, nil
}

// ListJobs returns all registered backup jobs sorted by name, their heartbeat URLs
// decrypted. A job whose heartbeat URL cannot be opened is returned without it (see
// openJob); only a job whose JSON cannot be read is skipped.
func (s *SQLiteStore) ListJobs(ctx context.Context) ([]*models.Job, error) {
	return listRecords(ctx, s, tableJobs, s.openJob, "SELECT id, data FROM jobs ORDER BY name, id")
}

// DeleteJob removes a job or returns ErrNotFound. Its backup records are kept; its
// RPO breaches and database join times go with it.
func (s *SQLiteStore) DeleteJob(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := execOne(ctx, tx, ErrNotFound, "DELETE FROM jobs WHERE id = ?", id); err != nil {
			return err
		}
		for _, q := range []string{"DELETE FROM rpo_breaches WHERE job_id = ?", "DELETE FROM job_database_joins WHERE job_id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("store: delete job %s: %w", id, err)
			}
		}
		return nil
	})
}

// SaveBackupRecord stores or updates a backup execution record.
func (s *SQLiteStore) SaveBackupRecord(ctx context.Context, record *models.BackupRecord) error {
	if record == nil || record.ID == "" {
		return fmt.Errorf("%w: backup record with ID is required", ErrInvalidRecord)
	}
	if record.Manifest != nil {
		// The manifest is stored apart from the record (see putManifest).
		return s.withTx(ctx, func(tx *sql.Tx) error {
			if err := putManifest(ctx, tx, record.ID, record.Manifest); err != nil {
				return err
			}
			return putBackup(ctx, tx, record)
		})
	}
	return putBackup(ctx, s.db, record)
}

// GetBackupRecord returns a single backup record or ErrNotFound.
func (s *SQLiteStore) GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error) {
	return getRecord[models.BackupRecord](ctx, s.db, ErrNotFound, "SELECT data FROM backups WHERE id = ?", id)
}

// ListBackupRecords returns all backup records, optionally filtered by database name,
// sorted by StartedAt descending (newest first).
func (s *SQLiteStore) ListBackupRecords(ctx context.Context, database string) ([]*models.BackupRecord, error) {
	if database == "" {
		return listRecords[models.BackupRecord](ctx, s, tableBackups, nil,
			"SELECT id, data FROM backups ORDER BY started_at DESC, id DESC")
	}
	return listRecords[models.BackupRecord](ctx, s, tableBackups, nil,
		"SELECT id, data FROM backups WHERE database_name = ? ORDER BY started_at DESC, id DESC", database)
}

// DeleteBackupRecord deletes a backup record (and its manifest) or returns ErrNotFound.
func (s *SQLiteStore) DeleteBackupRecord(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM backup_manifests WHERE backup_id = ?", id); err != nil {
			return fmt.Errorf("store: delete manifest of %s: %w", id, err)
		}
		return execOne(ctx, tx, ErrNotFound, "DELETE FROM backups WHERE id = ?", id)
	})
}

// SaveRestoreRecord stores or updates a disaster recovery restore execution record.
func (s *SQLiteStore) SaveRestoreRecord(ctx context.Context, record *models.RestoreRecord) error {
	if record == nil || record.ID == "" {
		return fmt.Errorf("%w: restore record with ID is required", ErrInvalidRecord)
	}
	return putRestore(ctx, s.db, record)
}

// GetRestoreRecord returns a single restore record or ErrNotFound.
func (s *SQLiteStore) GetRestoreRecord(ctx context.Context, id string) (*models.RestoreRecord, error) {
	return getRecord[models.RestoreRecord](ctx, s.db, ErrNotFound, "SELECT data FROM restores WHERE id = ?", id)
}

// DeleteRestoreRecord deletes a restore record or returns ErrNotFound. Only the history
// entry goes; the restored database is never touched.
func (s *SQLiteStore) DeleteRestoreRecord(ctx context.Context, id string) error {
	return execOne(ctx, s.db, ErrNotFound, "DELETE FROM restores WHERE id = ?", id)
}

// ListRestoreRecords returns all restore operations sorted by StartedAt descending.
func (s *SQLiteStore) ListRestoreRecords(ctx context.Context) ([]*models.RestoreRecord, error) {
	return listRecords[models.RestoreRecord](ctx, s, tableRestores, nil,
		"SELECT id, data FROM restores ORDER BY started_at DESC, id DESC")
}

// putJob upserts job and its indexed columns.
func putJob(ctx context.Context, e execer, job *models.Job) error {
	data, err := encode(job)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertJobSQL,
		job.ID, job.Name, job.Database, boolInt(job.Enabled), timeKey(job.CreatedAt), job.ConnectionID, job.StorageTargetID, data); err != nil {
		return fmt.Errorf("store: save job %s: %w", job.ID, err)
	}
	return nil
}

// putBackup upserts record and its indexed columns. Live progress is never stored.
func putBackup(ctx context.Context, e execer, record *models.BackupRecord) error {
	stored := *record
	stored.Progress = nil
	data, err := encode(&stored)
	if err != nil {
		return err
	}
	phases, err := encode(stored.Phases)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertBackupSQL,
		record.ID, record.JobID, record.Database, string(record.Status), timeKey(record.StartedAt), record.ConnectionID, record.StorageTargetID, record.RetryOf, record.SizeBytes, phases, record.RunID, data); err != nil {
		return fmt.Errorf("store: save backup record %s: %w", record.ID, err)
	}
	return nil
}

// putRestore upserts record and its indexed columns. Live progress is never stored.
func putRestore(ctx context.Context, e execer, record *models.RestoreRecord) error {
	stored := *record
	stored.Progress = nil
	data, err := encode(&stored)
	if err != nil {
		return err
	}
	phases, err := encode(stored.Phases)
	if err != nil {
		return err
	}
	// The verification column mirrors data.verification; NULL when none ran.
	var verification any
	if stored.Verification != nil {
		if verification, err = encode(stored.Verification); err != nil {
			return err
		}
	}
	if _, err := e.ExecContext(ctx, upsertRestoreSQL,
		record.ID, record.BackupID, record.SourceDatabase, record.TargetDatabase,
		string(record.Status), timeKey(record.StartedAt), phases, verification, data); err != nil {
		return fmt.Errorf("store: save restore record %s: %w", record.ID, err)
	}
	return nil
}
