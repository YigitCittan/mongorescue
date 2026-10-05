// Package store persists MongoRescue metadata (jobs, backup and restore history, and
// notification channels and rules) in an embedded SQLite database. The driver is pure
// Go, so the binary keeps building with CGO_ENABLED=0 and needs no external database.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// Sentinel errors for store operations.
var (
	// ErrNotFound is returned when a job or backup record does not exist.
	ErrNotFound = errors.New("store: record not found")
	// ErrInvalidRecord is returned when a record to save is nil or has no ID.
	ErrInvalidRecord = errors.New("store: invalid record")
	// ErrAlreadyExists is returned when creating a record whose ID is taken.
	ErrAlreadyExists = errors.New("store: a record with this id already exists")
	// ErrInvalidPath is returned when the database path cannot be used as an SQLite file name.
	ErrInvalidPath = errors.New("store: invalid database path")
	// ErrSchemaTooNew is returned when the database was migrated by a newer MongoRescue
	// release than the running one; downgrading the schema is not supported.
	ErrSchemaTooNew = errors.New("store: database schema is newer than this binary supports")
	// ErrMigrationChanged is returned when a migration applied to the database no
	// longer matches the SQL embedded in this binary (its recorded SHA-256 checksum
	// differs), or an applied migration is missing from the binary. The store is not
	// opened and nothing is written.
	ErrMigrationChanged = errors.New("store: an applied schema migration has changed")
	// ErrNoSecretBox is returned by operations on encrypted fields when the store was
	// opened without WithSecretBox.
	ErrNoSecretBox = errors.New("store: no secret key configured for encrypted fields")
	// ErrLegacyImport is returned when importing a legacy state.json file fails. The
	// file is left untouched and nothing is written to the database.
	ErrLegacyImport = errors.New("store: legacy state import failed")
	// ErrCorruptRecord is returned when a stored row cannot be read: its JSON does not
	// decode into the record type, or an encrypted field cannot be opened. Lists skip
	// such rows and report them through CorruptRecords instead.
	ErrCorruptRecord = errors.New("store: stored record cannot be read")
)

// Store defines the repository contract for MongoRescue metadata.
type Store interface {
	// SaveJob creates or replaces a job (imports and tests). It sets CreatedAt (when
	// zero) and UpdatedAt on the passed job. API and scheduler writes use CreateJob and
	// UpdateJob, which never overwrite or recreate a job by accident.
	SaveJob(ctx context.Context, job *models.Job) error
	// CreateJob inserts a new job, or returns ErrAlreadyExists when the ID is taken.
	// It sets CreatedAt (when zero) and UpdatedAt.
	CreateJob(ctx context.Context, job *models.Job) error
	// UpdateJob replaces an existing job, or returns ErrNotFound when it was deleted.
	// It sets UpdatedAt.
	UpdateJob(ctx context.Context, job *models.Job) error
	// UpdateJobRunTimes stores only a job's run timestamps (nil keeps the stored
	// value), never its settings or UpdatedAt, or returns ErrNotFound when the job
	// was deleted.
	UpdateJobRunTimes(ctx context.Context, id string, lastRun, nextRun *time.Time) error
	// GetJob returns a job or ErrNotFound.
	GetJob(ctx context.Context, id string) (*models.Job, error)
	// ListJobs returns all jobs sorted by name.
	ListJobs(ctx context.Context) ([]*models.Job, error)
	// DeleteJob removes a job or returns ErrNotFound.
	DeleteJob(ctx context.Context, id string) error
	// UpdateJobKnownDatabases stores only a job's KnownDatabases, as update decides
	// from the job as stored now (in one transaction), never its settings or
	// UpdatedAt, or returns ErrNotFound when the job was deleted.
	UpdateJobKnownDatabases(ctx context.Context, id string, update func(job *models.Job) (known []string, write bool)) error

	// SaveJobRun creates or replaces a job run.
	SaveJobRun(ctx context.Context, run *models.JobRun) error
	// GetJobRun returns a job run or ErrNotFound.
	GetJobRun(ctx context.Context, id string) (*models.JobRun, error)
	// ListJobRuns returns up to limit runs of a job, newest first.
	ListJobRuns(ctx context.Context, jobID string, limit int) ([]*models.JobRun, error)
	// ListExecutedJobRuns is ListJobRuns without skipped runs, limited after
	// leaving them out.
	ListExecutedJobRuns(ctx context.Context, jobID string, limit int) ([]*models.JobRun, error)
	// LatestJobRuns maps each of jobIDs that has runs to its newest run, in one
	// query (per 500 IDs).
	LatestJobRuns(ctx context.Context, jobIDs []string) (map[string]*models.JobRun, error)
	// ListRunningJobRuns returns every job run still recorded as running.
	ListRunningJobRuns(ctx context.Context) ([]*models.JobRun, error)

	// SaveBackupRecord creates or replaces a backup record.
	SaveBackupRecord(ctx context.Context, record *models.BackupRecord) error
	// GetBackupRecord returns a backup record or ErrNotFound.
	GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error)
	// ListBackupRecords returns backup records, newest first, optionally filtered by
	// database name (empty means all databases).
	ListBackupRecords(ctx context.Context, database string) ([]*models.BackupRecord, error)
	// DeleteBackupRecord removes a backup record or returns ErrNotFound.
	DeleteBackupRecord(ctx context.Context, id string) error

	// SaveRestoreRecord creates or replaces a restore record.
	SaveRestoreRecord(ctx context.Context, record *models.RestoreRecord) error
	// GetRestoreRecord returns a restore record or ErrNotFound.
	GetRestoreRecord(ctx context.Context, id string) (*models.RestoreRecord, error)
	// ListRestoreRecords returns all restore records, newest first.
	ListRestoreRecords(ctx context.Context) ([]*models.RestoreRecord, error)
	// DeleteRestoreRecord removes a restore record (history only; restored data is
	// never touched) or returns ErrNotFound.
	DeleteRestoreRecord(ctx context.Context, id string) error

	// QueryBackupRecords returns the backup records matching f, ordered and paged as f
	// asks, each with its newest retry, and the number of all matches. Out-of-range
	// paging or time fields return an ErrInvalidFilter error.
	QueryBackupRecords(ctx context.Context, f BackupFilter) (*BackupPage, error)
	// QueryRestoreRecords returns the restore records matching f, ordered and paged as
	// f asks, and the number of all matches. Out-of-range paging or time fields return
	// an ErrInvalidFilter error.
	QueryRestoreRecords(ctx context.Context, f RestoreFilter) (*RestorePage, error)
	// ListBackupDatabases returns the distinct database names of all backups, sorted.
	ListBackupDatabases(ctx context.Context) ([]string, error)
	// ListRestoreDatabases returns the distinct target databases of all restores,
	// sorted.
	ListRestoreDatabases(ctx context.Context) ([]string, error)
	// ListBackupDatabasesIn is ListBackupDatabases for the backups taken from the
	// connections in set (every backup when set is nil).
	ListBackupDatabasesIn(ctx context.Context, set auth.ConnectionSet) ([]string, error)
	// ListRestoreDatabasesIn is ListRestoreDatabases for the restores whose source
	// and target connections are in set (every restore when set is nil).
	ListRestoreDatabasesIn(ctx context.Context, set auth.ConnectionSet) ([]string, error)
	// ListBackupTargetsIn returns the distinct storage target IDs of the backups taken
	// from the connections in set (every backup when set is nil), sorted.
	ListBackupTargetsIn(ctx context.Context, set auth.ConnectionSet) ([]string, error)
	// BackupStats returns SQL aggregates over every backup: counts by status, failures
	// started at or after since, the size of completed backups and the newest backup.
	BackupStats(ctx context.Context, since time.Time) (*BackupStats, error)
	// BackupStatsIn is BackupStats over the backups taken from the connections in set
	// (every backup when set is nil).
	BackupStatsIn(ctx context.Context, since time.Time, set auth.ConnectionSet) (*BackupStats, error)
	// LatestJobBackups maps every job ID with backups to its newest backup, only
	// considering backups in status when it is not empty.
	LatestJobBackups(ctx context.Context, status models.BackupStatus) (map[string]*models.BackupRecord, error)
	// LatestJobBackupsIn is LatestJobBackups over the backups taken from the
	// connections in set (every backup when set is nil).
	LatestJobBackupsIn(ctx context.Context, status models.BackupStatus, set auth.ConnectionSet) (map[string]*models.BackupRecord, error)
	// LatestJobDatabaseBackups maps every database of a job with backups in status
	// (any when empty) to its newest one.
	LatestJobDatabaseBackups(ctx context.Context, jobID string, status models.BackupStatus) (map[string]*models.BackupRecord, error)
	// RestoreStats returns the number of restores and their counts by status.
	RestoreStats(ctx context.Context) (*RestoreStats, error)
	// RestoreStatsIn is RestoreStats over the restores whose source and target
	// connections are in set (every restore when set is nil).
	RestoreStatsIn(ctx context.Context, set auth.ConnectionSet) (*RestoreStats, error)
	// BackupHistory returns SQL aggregates of the backups over time: outcomes and
	// sizes per day, each job's newest runs and last success, and failed
	// verifications (see BackupHistoryQuery).
	BackupHistory(ctx context.Context, q BackupHistoryQuery) (*BackupHistory, error)

	// CorruptRecords returns the stored rows that lists skipped because they cannot be
	// read (see CorruptRecord), after checking each again. Rows are never changed.
	CorruptRecords(ctx context.Context) ([]CorruptRecord, error)
}
