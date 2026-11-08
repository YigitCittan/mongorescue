package operations

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// The ports below are the parts of the metadata store the use cases need beyond
// store.Store. They are small and named so that a backend (SQLite today) and the
// test fakes must implement each one: a missing method fails to compile instead of
// silently switching a safety feature off.

// BackupUpdater changes a backup record atomically.
type BackupUpdater interface {
	// UpdateBackupRecord applies fn to the stored record in one transaction and
	// returns the stored result, or store.ErrNotFound. An error from fn aborts it.
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
}

// ManifestReader reads the manifests kept apart from the backup records.
type ManifestReader interface {
	// GetManifest returns the manifest of backup id, or store.ErrNotFound.
	GetManifest(ctx context.Context, id string) (*models.Manifest, error)
}

// RetentionLogReader reads the log of backups the retention deleted.
type RetentionLogReader interface {
	// ListRetentionLog returns up to limit entries of job jobID, newest first.
	ListRetentionLog(ctx context.Context, jobID string, limit int) ([]*models.RetentionLogEntry, error)
}

// ApprovalStore persists approval requests and the secret kept apart from one.
type ApprovalStore interface {
	// CreateApproval stores a new approval request.
	CreateApproval(ctx context.Context, a *models.Approval) error
	// GetApproval returns request id, or store.ErrNotFound.
	GetApproval(ctx context.Context, id string) (*models.Approval, error)
	// ListApprovals returns up to limit requests in status ("" for all), newest first.
	ListApprovals(ctx context.Context, status models.ApprovalStatus, limit int) ([]*models.Approval, error)
	// UpdateApproval applies fn to request id in one transaction.
	UpdateApproval(ctx context.Context, id string, fn func(*models.Approval) error) (*models.Approval, error)
	// ApprovalSecret returns the secret of request id (cleared once it is decided).
	ApprovalSecret(ctx context.Context, id string) (string, error)
}

// PendingChangeStore persists the lowered protections waiting to take effect.
type PendingChangeStore interface {
	// ReplacePendingChange stores c, replacing a change of the same kind and subject.
	ReplacePendingChange(ctx context.Context, c *models.PendingChange) (*models.PendingChange, error)
	// ListPendingChanges returns every pending change, the earliest first.
	ListPendingChanges(ctx context.Context) ([]*models.PendingChange, error)
	// GetPendingChange returns change id, or store.ErrNotFound.
	GetPendingChange(ctx context.Context, id string) (*models.PendingChange, error)
	// DeletePendingChange removes change id, or returns store.ErrNotFound.
	DeletePendingChange(ctx context.Context, id string) error
	// DeletePendingChangesOf removes the changes of a kind and subject and reports
	// whether there were any.
	DeletePendingChangesOf(ctx context.Context, kind models.PendingChangeKind, subject string) (bool, error)
}

// UserLister lists the user accounts.
type UserLister interface {
	// ListUsers returns every user.
	ListUsers(ctx context.Context) ([]*auth.User, error)
}

// JobIndex answers questions about the jobs' stored last status.
type JobIndex interface {
	// HasForeignLastBackups reports whether a job of the connections in set has its
	// newest backup on a connection outside set.
	HasForeignLastBackups(ctx context.Context, set auth.ConnectionSet) (bool, error)
}

// ChangeStamper counts the writes to the tables the overview and statistics read.
type ChangeStamper interface {
	// ChangeStamp returns the number of those writes so far.
	ChangeStamp(ctx context.Context) (int64, error)
}

// Store is everything the operations service needs from the metadata store:
// store.Store and the ports above. Config.Store is a Store, so a backend that lacks
// one of them does not compile.
type Store interface {
	store.Store
	BackupUpdater
	ManifestReader
	RetentionLogReader
	ApprovalStore
	PendingChangeStore
	UserLister
	JobIndex
	ChangeStamper
}

var _ Store = (*store.SQLiteStore)(nil)
