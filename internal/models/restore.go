package models

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// ErrInPlaceNotConfirmed is returned when a restore would write into a non-clone
// namespace (the source database or an explicit target_database) without both
// "safe_clone": false and "confirm_in_place": true.
var ErrInPlaceNotConfirmed = errors.New(`in-place restore not confirmed: set "safe_clone": false and "confirm_in_place": true`)

// RestoreStatus represents the execution state of a restore operation.
type RestoreStatus string

const (
	// RestoreStatusPending indicates the restore is queued.
	RestoreStatusPending RestoreStatus = "pending"

	// RestoreStatusInProgress indicates data is streaming into mongorestore.
	RestoreStatusInProgress RestoreStatus = "in_progress"

	// RestoreStatusCompleted indicates the restore succeeded.
	RestoreStatusCompleted RestoreStatus = "completed"

	// RestoreStatusFailed indicates restore failure.
	RestoreStatusFailed RestoreStatus = "failed"
)

// VerifyPolicy controls when a restore first verifies the backup artifact end to end
// (checksum and, for encrypted backups, full age authentication) before mongorestore
// is started.
type VerifyPolicy string

const (
	// VerifyAlways verifies before every restore unless the request opts out.
	VerifyAlways VerifyPolicy = "always"

	// VerifyAuto verifies only in-place restores (see RestoreRequest.InPlace), unless
	// the request decides explicitly. This is the default.
	VerifyAuto VerifyPolicy = "auto"

	// VerifyNever skips verification unless the request opts in.
	VerifyNever VerifyPolicy = "never"
)

// Valid reports whether p is one of the defined policies.
func (p VerifyPolicy) Valid() bool {
	switch p {
	case VerifyAlways, VerifyAuto, VerifyNever:
		return true
	default:
		return false
	}
}

// RestoreRequest encapsulates parameters for a disaster recovery or restore operation.
type RestoreRequest struct {
	// BackupID is the unique identifier of the backup artifact to restore.
	BackupID string `json:"backup_id"`

	// TargetDatabase specifies a destination database name for an in-place (non-clone)
	// restore. Setting it requires SafeClone=false and ConfirmInPlace=true.
	TargetDatabase string `json:"target_database,omitempty"`

	// SafeClone routes the restore into an isolated clone namespace
	// (<db>_rescue_<timestamp>). Nil (omitted) means true: a restore never overwrites
	// existing data unless the caller sets it to false explicitly. Use IsSafeClone.
	SafeClone *bool `json:"safe_clone,omitempty"`

	// ConfirmInPlace must be true, together with SafeClone=false, to restore into the
	// source database or an explicit TargetDatabase. See ErrInPlaceNotConfirmed.
	ConfirmInPlace bool `json:"confirm_in_place,omitempty"`

	// DropTarget, if true, instructs mongorestore to drop collections before importing.
	// MUST be explicitly confirmed to prevent data loss.
	DropTarget bool `json:"drop_target"`

	// SelectedCollections restricts restore to a specific subset of collections.
	SelectedCollections []string `json:"selected_collections,omitempty"`

	// DryRun validates headers, connection, and namespace parameters without writing data.
	DryRun bool `json:"dry_run"`

	// TargetConnectionID selects the Connection to restore into. Empty means the
	// connection the backup was taken from; a different one restores across servers.
	TargetConnectionID string `json:"target_connection_id,omitempty"`

	// TargetConnectionName is the resolved name of the target connection, recorded on
	// the restore record. It is filled by the server, never read from clients.
	TargetConnectionName string `json:"-"`

	// MongoURI is the resolved connection string of the target connection. It is
	// filled by the server and never read from or written to JSON.
	MongoURI string `json:"-"`

	// Verify, when set, explicitly enables or disables verify-before-restore: a first
	// pass streams the whole artifact to check its SHA-256 (and decrypt it) before
	// mongorestore starts. When nil, the server's VerifyPolicy decides.
	Verify *bool `json:"verify,omitempty"`
}

// IsSafeClone reports whether the restore targets a fresh clone namespace. An omitted
// SafeClone defaults to true.
func (r RestoreRequest) IsSafeClone() bool {
	return r.SafeClone == nil || *r.SafeClone
}

// InPlace reports whether the restore would write into a pre-named namespace (the
// source database or TargetDatabase) instead of a fresh <db>_rescue_<timestamp> clone.
func (r RestoreRequest) InPlace() bool {
	return !r.IsSafeClone() || strings.TrimSpace(r.TargetDatabase) != ""
}

// ValidateTarget enforces the safe-clone default: an in-place restore requires an
// explicit "safe_clone": false and "confirm_in_place": true, otherwise it returns an
// error wrapping ErrInPlaceNotConfirmed.
func (r RestoreRequest) ValidateTarget() error {
	if !r.InPlace() {
		return nil
	}
	if r.IsSafeClone() {
		return fmt.Errorf("%w (target_database is only honoured for in-place restores)", ErrInPlaceNotConfirmed)
	}
	if !r.ConfirmInPlace {
		return ErrInPlaceNotConfirmed
	}
	return nil
}

// ShouldVerify resolves whether this request must be verified before restoring.
// An explicit Verify wins; otherwise VerifyAlways/VerifyNever apply, and VerifyAuto
// (or an unknown policy) verifies exactly when the restore is in place (not a clone).
func (r RestoreRequest) ShouldVerify(policy VerifyPolicy) bool {
	if r.Verify != nil {
		return *r.Verify
	}
	switch policy {
	case VerifyAlways:
		return true
	case VerifyNever:
		return false
	default:
		return r.InPlace()
	}
}

// RestoreRecord stores the history and outcome of a restore attempt.
type RestoreRecord struct {
	// ID is the unique identifier for this restore execution.
	ID string `json:"id"`

	// BackupID references the source backup record.
	BackupID string `json:"backup_id"`

	// SourceDatabase is the original database name contained in the archive.
	SourceDatabase string `json:"source_database"`

	// TargetDatabase is the actual database name where data was restored.
	TargetDatabase string `json:"target_database"`

	// SourceConnectionID references the Connection the backup was taken from.
	SourceConnectionID string `json:"source_connection_id"`

	// SourceConnectionName is a snapshot, at restore time, of the source connection's
	// name (the backup's name snapshot when the connection no longer exists).
	SourceConnectionName string `json:"source_connection_name"`

	// TargetConnectionID references the Connection restored into.
	TargetConnectionID string `json:"target_connection_id"`

	// TargetConnectionName is a snapshot of the target connection's name.
	TargetConnectionName string `json:"target_connection_name"`

	// Status indicates if the restore succeeded or failed.
	Status RestoreStatus `json:"status"`

	// StartedAt marks the start of the restore stream.
	StartedAt time.Time `json:"started_at"`

	// CompletedAt marks the finish timestamp.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// DurationSeconds tracks total execution time.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`

	// ErrorMessage holds error details in case of failure.
	ErrorMessage string `json:"error_message,omitempty"`

	// DryRun indicates whether this was a simulation run.
	DryRun bool `json:"dry_run"`

	// Verified reports that the artifact passed verify-before-restore (checksum and,
	// if encrypted, full authentication) before mongorestore was started.
	Verified bool `json:"verified,omitempty"`
}

// Redacted returns a copy of the request with the MongoURI password masked, suitable
// for logging or echoing back to API clients. Slice fields are cloned; the receiver
// is not modified.
func (r RestoreRequest) Redacted() RestoreRequest {
	r.SelectedCollections = slices.Clone(r.SelectedCollections)
	r.MongoURI = redact.URI(r.MongoURI)
	if r.SafeClone != nil {
		v := *r.SafeClone
		r.SafeClone = &v
	}
	if r.Verify != nil {
		v := *r.Verify
		r.Verify = &v
	}
	return r
}
