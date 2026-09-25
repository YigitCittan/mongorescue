package store

import (
	"context"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

const (
	upsertJobSQL = `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, storage_target_id, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, database_name = excluded.database_name,
			enabled = excluded.enabled, created_at = excluded.created_at,
			connection_id = excluded.connection_id, storage_target_id = excluded.storage_target_id,
			data = excluded.data`

	upsertBackupSQL = `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET job_id = excluded.job_id, database_name = excluded.database_name,
			status = excluded.status, started_at = excluded.started_at,
			connection_id = excluded.connection_id, storage_target_id = excluded.storage_target_id,
			data = excluded.data`

	upsertRestoreSQL = `INSERT INTO restores (id, backup_id, source_database, target_database, status, started_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET backup_id = excluded.backup_id, source_database = excluded.source_database,
			target_database = excluded.target_database, status = excluded.status,
			started_at = excluded.started_at, data = excluded.data`
)

// SaveJob creates or updates a scheduled backup job. It sets CreatedAt (when zero) and
// UpdatedAt on job before persisting it.
func (s *SQLiteStore) SaveJob(ctx context.Context, job *models.Job) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("%w: job with ID is required", ErrInvalidRecord)
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	return putJob(ctx, s.db, job)
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
	data, err := encode(job)
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
	data, err := encode(job)
	if err != nil {
		return err
	}
	return execOne(ctx, s.db, ErrNotFound, `UPDATE jobs SET name = ?, database_name = ?, enabled = ?, created_at = ?,
		connection_id = ?, storage_target_id = ?, data = ? WHERE id = ?`,
		job.Name, job.Database, boolInt(job.Enabled), timeKey(job.CreatedAt), job.ConnectionID, job.StorageTargetID, data, job.ID)
}

// GetJob returns a job by ID or ErrNotFound.
func (s *SQLiteStore) GetJob(ctx context.Context, id string) (*models.Job, error) {
	return getRecord[models.Job](ctx, s.db, ErrNotFound, "SELECT data FROM jobs WHERE id = ?", id)
}

// ListJobs returns all registered backup jobs sorted by name.
func (s *SQLiteStore) ListJobs(ctx context.Context) ([]*models.Job, error) {
	return listRecords[models.Job](ctx, s.db, "SELECT data FROM jobs ORDER BY name, id")
}

// DeleteJob removes a job or returns ErrNotFound. Its backup records are kept.
func (s *SQLiteStore) DeleteJob(ctx context.Context, id string) error {
	return execOne(ctx, s.db, ErrNotFound, "DELETE FROM jobs WHERE id = ?", id)
}

// SaveBackupRecord stores or updates a backup execution record.
func (s *SQLiteStore) SaveBackupRecord(ctx context.Context, record *models.BackupRecord) error {
	if record == nil || record.ID == "" {
		return fmt.Errorf("%w: backup record with ID is required", ErrInvalidRecord)
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
		return listRecords[models.BackupRecord](ctx, s.db,
			"SELECT data FROM backups ORDER BY started_at DESC, id DESC")
	}
	return listRecords[models.BackupRecord](ctx, s.db,
		"SELECT data FROM backups WHERE database_name = ? ORDER BY started_at DESC, id DESC", database)
}

// DeleteBackupRecord deletes a backup record or returns ErrNotFound.
func (s *SQLiteStore) DeleteBackupRecord(ctx context.Context, id string) error {
	return execOne(ctx, s.db, ErrNotFound, "DELETE FROM backups WHERE id = ?", id)
}

// SaveRestoreRecord stores or updates a disaster recovery restore execution record.
func (s *SQLiteStore) SaveRestoreRecord(ctx context.Context, record *models.RestoreRecord) error {
	if record == nil || record.ID == "" {
		return fmt.Errorf("%w: restore record with ID is required", ErrInvalidRecord)
	}
	return putRestore(ctx, s.db, record)
}

// ListRestoreRecords returns all restore operations sorted by StartedAt descending.
func (s *SQLiteStore) ListRestoreRecords(ctx context.Context) ([]*models.RestoreRecord, error) {
	return listRecords[models.RestoreRecord](ctx, s.db,
		"SELECT data FROM restores ORDER BY started_at DESC, id DESC")
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

// putBackup upserts record and its indexed columns.
func putBackup(ctx context.Context, e execer, record *models.BackupRecord) error {
	data, err := encode(record)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertBackupSQL,
		record.ID, record.JobID, record.Database, string(record.Status), timeKey(record.StartedAt), record.ConnectionID, record.StorageTargetID, data); err != nil {
		return fmt.Errorf("store: save backup record %s: %w", record.ID, err)
	}
	return nil
}

// putRestore upserts record and its indexed columns.
func putRestore(ctx context.Context, e execer, record *models.RestoreRecord) error {
	data, err := encode(record)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertRestoreSQL,
		record.ID, record.BackupID, record.SourceDatabase, record.TargetDatabase,
		string(record.Status), timeKey(record.StartedAt), data); err != nil {
		return fmt.Errorf("store: save restore record %s: %w", record.ID, err)
	}
	return nil
}
