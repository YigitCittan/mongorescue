package models

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// ErrInPlaceNotConfirmed is returned when a restore would write into a non-clone
// namespace (the source database or an explicit target_database) without both
// "safe_clone": false and "confirm_in_place": true.
var ErrInPlaceNotConfirmed = errors.New(`in-place restore not confirmed: set "safe_clone": false and "confirm_in_place": true`)

// ErrUsersAndRolesNotAllowed is returned when a restore asks for
// "restore_users_and_roles" but is not an in-place restore into the backup's own
// database: mongorestore restores users and roles under the source database name, so
// a safe clone or a renamed target would get the users of another database.
var ErrUsersAndRolesNotAllowed = errors.New(`"restore_users_and_roles" is only allowed for an in-place restore into the backup's own database`)

// ErrNoUsersAndRoles is returned when a restore asks for "restore_users_and_roles" but
// the backup was taken without its database's users and roles.
var ErrNoUsersAndRoles = errors.New("the backup does not contain users and roles")

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

	// RestoreStatusCancelled indicates the restore was stopped before it finished. A
	// cancelled safe clone is dropped; a cancelled in-place restore may have left the
	// target partially restored (see RestoreRecord.Warning).
	RestoreStatusCancelled RestoreStatus = "cancelled"
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

	// RestoreUsersAndRoles restores the users and roles stored in the archive into the
	// source database (mongorestore --restoreDbUsersAndRoles), replacing the users and
	// roles defined there. It needs an in-place restore into the backup's own database
	// and a backup taken with them; see ValidateUsersAndRoles.
	RestoreUsersAndRoles bool `json:"restore_users_and_roles,omitempty"`

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

	// VerifyRestore, when true, compares the restored database with the manifest
	// captured at backup time once the restore succeeded: per collection, the document
	// count and the indexes (see RestoreRecord.Verification). It is off when omitted,
	// which keeps API clients of earlier releases unchanged; the dashboard turns it on.
	// A failed verification keeps the restore completed and adds a warning.
	VerifyRestore bool `json:"verify_restore,omitempty"`

	// Force starts the restore although its preflight failed (see PreflightResult).
	// Warnings never block a restore; Force only overrides failed checks.
	Force bool `json:"force,omitempty"`

	// CloneDatabase, when set, names the safe clone instead of the default
	// <db>_rescue_<timestamp> (automated restore tests restore into
	// <db>_rescue_verify_<timestamp>). It only applies to safe-clone restores and is
	// set by code, never read from clients.
	CloneDatabase string `json:"-"`

	// PITR makes the request a point-in-time restore of a PITR stream: a base
	// backup is restored into safe clones and the stream's oplog is replayed up to
	// the target. BackupID is then left empty (the base is chosen by the plan), and
	// in-place restores, dry runs, collection selections, drop_target and users and
	// roles are refused (see ValidatePITR).
	PITR *PITRTarget `json:"pitr,omitempty"`

	// Databases restricts a point-in-time restore to these databases; empty
	// restores every database of the instance except admin, config and local.
	Databases []string `json:"databases,omitempty"`

	// PITRCloneSuffix, when set, replaces the "_rescue_<timestamp>" suffix of the
	// databases of a point-in-time restore (chain tests use
	// "_rescue_verify_<timestamp>_<hex>"). It is set by code, never read from
	// clients.
	PITRCloneSuffix string `json:"-"`

	// PostRestoreCommands are the target connection's post-restore commands
	// (Connection.PostRestoreCommands), run against the databases a safe-clone
	// restore created before it is reported complete. They are set by code from
	// the target connection, never read from clients.
	PostRestoreCommands []PostRestoreCommand `json:"-"`

	// DeferPostRestore leaves the post-restore commands to the caller, which runs
	// them through the restore engine's RunPostRestore once it has compared the
	// restored data with the backup (restore verification, restore tests). Set by
	// code, never read from clients.
	DeferPostRestore bool `json:"-"`
}

// ErrPITRInPlace is returned for a point-in-time restore that asks to write into
// existing databases: this release restores to a point in time into safe clones only.
var ErrPITRInPlace = errors.New("point-in-time restores go into safe clones only; in-place point-in-time restores are not offered")

// PITRTarget is the stream and the moment of a point-in-time restore. Exactly one of
// At and TS is set.
type PITRTarget struct {
	// StreamID is the PITR stream to restore from.
	StreamID string `json:"stream_id"`
	// At restores every write up to and including this second (RFC 3339; the
	// primary's clock).
	At *time.Time `json:"at,omitempty"`
	// TS restores every write before this exact oplog position, which is not
	// applied.
	TS *pitr.Timestamp `json:"ts,omitempty"`
}

// Target returns the plan target of t.
func (t PITRTarget) Target() pitr.Target {
	var out pitr.Target
	if t.At != nil {
		out.At = *t.At
	}
	if t.TS != nil {
		ts := *t.TS
		out.TS = &ts
	}
	return out
}

// ValidatePITR checks a point-in-time request: a stream, exactly one of a time and a
// timestamp, valid database names other than admin, config and local, and none of
// the options a point-in-time restore does not offer (an in-place restore wraps
// ErrPITRInPlace). It returns nil for a request without PITR.
func (r RestoreRequest) ValidatePITR() error {
	if r.PITR == nil {
		if len(r.Databases) > 0 {
			return errors.New(`"databases" is only used by point-in-time restores ("pitr")`)
		}
		return nil
	}
	switch {
	case strings.TrimSpace(r.PITR.StreamID) == "":
		return errors.New("pitr.stream_id is required")
	case (r.PITR.At == nil) == (r.PITR.TS == nil):
		return errors.New("set exactly one of pitr.at and pitr.ts")
	case r.InPlace() || r.ConfirmInPlace:
		return ErrPITRInPlace
	case strings.TrimSpace(r.BackupID) != "":
		return errors.New("a point-in-time restore chooses its base backup itself; omit backup_id")
	case r.DryRun:
		return errors.New("dry runs are not offered for point-in-time restores; run the preflight instead")
	case r.DropTarget:
		return errors.New("drop_target does not apply to point-in-time restores, which create new databases")
	case len(r.SelectedCollections) > 0:
		return errors.New("selected_collections is not offered for point-in-time restores; use databases")
	case r.RestoreUsersAndRoles:
		return errors.New("restore_users_and_roles is not offered for point-in-time restores")
	}
	if _, err := r.PITR.Target().Limit(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, db := range r.Databases {
		db = strings.TrimSpace(db)
		if err := ValidateDatabaseName(db); err != nil {
			return fmt.Errorf("databases: %w", err)
		}
		if db == AdminDatabase || db == "config" || db == "local" {
			return fmt.Errorf("databases: %s is never restored to a point in time", db)
		}
		if IsRescueClone(db) {
			return fmt.Errorf("databases: %s is a database MongoRescue restored into; point-in-time restores leave clones out", db)
		}
		if seen[db] {
			return fmt.Errorf("databases: %s is listed twice", db)
		}
		seen[db] = true
	}
	return nil
}

// PITRDatabases returns the trimmed database selection of a point-in-time request.
func (r RestoreRequest) PITRDatabases() []string {
	var out []string
	for _, db := range r.Databases {
		if db = strings.TrimSpace(db); db != "" {
			out = append(out, db)
		}
	}
	return out
}

// PITRRestore records what a point-in-time restore replayed (RestoreRecord.PITR).
type PITRRestore struct {
	// StreamID, ChainID and BaseID are the stream, the chain and the base backup
	// the plan chose.
	StreamID string `json:"stream_id"`
	ChainID  string `json:"chain_id"`
	BaseID   string `json:"base_id"`
	// TargetTime is the moment restored to and Limit the exclusive end of the
	// replay (--oplogLimit).
	TargetTime time.Time      `json:"target_time"`
	Limit      pitr.Timestamp `json:"limit"`
	// Chunks and OplogBytes count the oplog chunks replayed and their stored size;
	// BaseBytes is the stored size of the base.
	Chunks     int   `json:"chunks"`
	OplogBytes int64 `json:"oplog_bytes"`
	BaseBytes  int64 `json:"base_bytes"`
	// Databases is the database selection; empty means the whole instance (every
	// database but admin, config and local).
	Databases []string `json:"databases,omitempty"`
	// CloneSuffix is appended to the name of every restored database
	// ("_rescue_<YYYYMMDD_HHMMSS>").
	CloneSuffix string `json:"clone_suffix"`
	// Clones are the databases this restore creates, recorded before it writes
	// anything and extended as it goes. Clean-up after a failure, a cancellation or
	// an interruption drops exactly these names, never a pattern.
	Clones []string `json:"clones,omitempty"`
	// OpsReplayed counts the operations the oplog filter wrote and OpsApplied the
	// ones mongorestore reported ("applied N oplog entries"; nil when it printed no
	// count).
	OpsReplayed int64  `json:"ops_replayed"`
	OpsApplied  *int64 `json:"ops_applied,omitempty"`
	// OpsUnverified is set when mongorestore printed no applied count, so the
	// replay could not be cross-checked (the record also carries a warning).
	OpsUnverified bool `json:"ops_unverified,omitempty"`
	// ChainTest marks the restore of a scheduled chain test.
	ChainTest bool `json:"chain_test,omitempty"`
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

// ValidateUsersAndRoles checks RestoreUsersAndRoles against the backup to restore:
// the restore must be in place (not a safe clone) into source's own database (no
// rename through target_database), and source must contain users and roles. Errors
// wrap ErrUsersAndRolesNotAllowed or ErrNoUsersAndRoles. Without
// RestoreUsersAndRoles it returns nil.
func (r RestoreRequest) ValidateUsersAndRoles(source *BackupRecord) error {
	if !r.RestoreUsersAndRoles {
		return nil
	}
	if r.IsSafeClone() {
		return fmt.Errorf(`%w: a safe clone restores into a new database; set "safe_clone": false and "confirm_in_place": true`, ErrUsersAndRolesNotAllowed)
	}
	if source == nil {
		return fmt.Errorf("%w: unknown source backup", ErrNoUsersAndRoles)
	}
	if target := strings.TrimSpace(r.TargetDatabase); target != "" && target != source.Database {
		return fmt.Errorf("%w: target_database %q renames the restore away from %q; omit target_database", ErrUsersAndRolesNotAllowed, target, source.Database)
	}
	if source.Database == AdminDatabase {
		return fmt.Errorf("%w: the admin database keeps users and roles as regular data; restore it without restore_users_and_roles", ErrUsersAndRolesNotAllowed)
	}
	if !source.UsersAndRoles {
		return fmt.Errorf("%w: backup %s was taken without include_users_and_roles", ErrNoUsersAndRoles, source.ID)
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

	// SelectedCollections lists the collections a selective restore restored; empty
	// means the whole database.
	SelectedCollections []string `json:"selected_collections,omitempty"`

	// Verified reports that the artifact passed verify-before-restore (checksum and,
	// if encrypted, full authentication) before mongorestore was started.
	Verified bool `json:"verified,omitempty"`

	// Warning notes something the restore could not check although it succeeded,
	// e.g. "document counts unavailable" or an in-place restore of a backup without a
	// recorded checksum, or that a cancelled in-place restore left partial data.
	Warning string `json:"warning,omitempty"`

	// InPlace reports that the restore writes into an existing namespace instead of a
	// fresh safe clone.
	InPlace bool `json:"in_place,omitempty"`

	// UsersAndRoles reports that the restore also restored the users and roles of the
	// source database (RestoreRequest.RestoreUsersAndRoles).
	UsersAndRoles bool `json:"users_and_roles,omitempty"`

	// CancelledBy names who cancelled the restore when Status is
	// RestoreStatusCancelled.
	CancelledBy string `json:"cancelled_by,omitempty"`

	// CancelledAt is when the cancellation was requested.
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`

	// Phases holds the timestamps of the run's phases.
	Phases RunPhases `json:"phases,omitzero"`

	// Preflight is the go/no-go summary of the checks that ran before the restore
	// started; nil when none ran (restores of earlier releases, restore tests).
	Preflight *PreflightResult `json:"preflight,omitempty"`

	// Forced reports that the restore was started with "force": true although a
	// preflight check failed.
	Forced bool `json:"forced,omitempty"`

	// Verification is the comparison of the restored database with the backup's
	// manifest (RestoreRequest.VerifyRestore); nil when it was not requested.
	Verification *RestoreVerification `json:"verification,omitempty"`

	// Progress is the live progress of a running restore. It is filled in API
	// responses only and never stored.
	Progress *RunProgress `json:"progress,omitempty"`

	// PITR describes a point-in-time restore: the base, the chain and the oplog it
	// replayed. Nil for the restore of a backup.
	PITR *PITRRestore `json:"pitr,omitempty"`

	// PostRestore is what the target connection's post-restore commands did (see
	// RestoreRequest.PostRestoreCommands); nil when the connection has none.
	PostRestore *PostRestoreReport `json:"post_restore,omitempty"`
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
