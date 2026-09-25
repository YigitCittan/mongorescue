// Package store persists MongoRescue metadata (jobs, backup and restore history, and
// notification channels and rules) in an embedded SQLite database. The driver is pure
// Go, so the binary keeps building with CGO_ENABLED=0 and needs no external database.
package store

import (
	"context"
	"errors"

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
	// ErrNoSecretBox is returned by operations on encrypted fields when the store was
	// opened without WithSecretBox.
	ErrNoSecretBox = errors.New("store: no secret key configured for encrypted fields")
	// ErrLegacyImport is returned when importing a legacy state.json file fails. The
	// file is left untouched and nothing is written to the database.
	ErrLegacyImport = errors.New("store: legacy state import failed")
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
	// GetJob returns a job or ErrNotFound.
	GetJob(ctx context.Context, id string) (*models.Job, error)
	// ListJobs returns all jobs sorted by name.
	ListJobs(ctx context.Context) ([]*models.Job, error)
	// DeleteJob removes a job or returns ErrNotFound.
	DeleteJob(ctx context.Context, id string) error

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
}
