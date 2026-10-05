package models

import (
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// BackupStatus represents the lifecycle state of a backup operation.
type BackupStatus string

const (
	// StatusPending indicates the backup job is enqueued but not yet executing.
	StatusPending BackupStatus = "pending"

	// StatusInProgress indicates the backup stream is actively running.
	StatusInProgress BackupStatus = "in_progress"

	// StatusCompleted indicates the backup completed and was verified successfully.
	StatusCompleted BackupStatus = "completed"

	// StatusFailed indicates the backup encountered an unrecoverable error.
	StatusFailed BackupStatus = "failed"

	// StatusPruned indicates the backup was deleted per retention policy.
	StatusPruned BackupStatus = "pruned"

	// StatusCancelled indicates the backup was stopped before it finished (by a user,
	// an API key or the application quitting); its partial artifact was removed. It
	// is not a failure: failure counts and failure alerts ignore it.
	StatusCancelled BackupStatus = "cancelled"

	// StatusSkipped is the outcome of a database of a run that was not backed up
	// because another backup of it was already running when the run started
	// (JobRunDatabase only; no backup record has it). It is neither a success nor a
	// failure: run outcomes, failure counts and failure alerts ignore it.
	StatusSkipped BackupStatus = "skipped"

	// StatusMissing indicates a completed backup whose archive a storage scan no
	// longer found on its target. A later scan that finds it again restores
	// StatusCompleted; nothing is deleted automatically.
	StatusMissing BackupStatus = "missing"

	// StatusDeleted indicates a backup deleted by a user, an API key or retention
	// whose archive is kept until PurgeAfter (the delete grace period): it is hidden
	// from lists by default, cannot be restored or verified, and can be undeleted,
	// which restores StatusBeforeDelete.
	StatusDeleted BackupStatus = "deleted"

	// StatusPurged indicates a deleted backup whose grace period ended: the purge
	// removed its archive (unless another record still names it). The record stays
	// as history.
	StatusPurged BackupStatus = "purged"
)

// Deleted reports whether a backup in state st was deleted (StatusDeleted, waiting
// for its purge, or StatusPurged): it cannot be restored, verified, pinned or deleted
// again.
func (st BackupStatus) Deleted() bool {
	return st == StatusDeleted || st == StatusPurged
}

// BackupTrigger records how a backup was started. Retention only ever prunes
// scheduled backups of the job that runs it; the others are kept until an admin
// deletes them.
type BackupTrigger string

// Backup triggers.
const (
	// TriggerScheduled is a cron-triggered run of a job.
	TriggerScheduled BackupTrigger = "scheduled"
	// TriggerOnDemand is a job run started through the REST API or the dashboard
	// (POST /api/v1/jobs/{id}/run).
	TriggerOnDemand BackupTrigger = "on_demand"
	// TriggerManual is a one-off backup started through the REST API or the
	// dashboard (POST /api/v1/backups).
	TriggerManual BackupTrigger = "manual"
	// TriggerMCP is a backup or job run started by an AI assistant through MCP.
	TriggerMCP BackupTrigger = "mcp"
)

// BackupRecord represents a persistent record of a completed or running backup.
type BackupRecord struct {
	// ID is the unique identifier for this backup (e.g. "bkp_20260924_153000_mydb").
	ID string `json:"id"`

	// JobID references the scheduled job that triggered this backup, if any.
	JobID string `json:"job_id,omitempty"`

	// RunID groups the backups of one job run: every database a run backs up gets
	// its own record with the run's ID (see JobRun). Empty for backups that are not
	// part of a job run (manual backups, and job backups taken before runs existed).
	RunID string `json:"run_id,omitempty"`

	// Trigger records how the backup was started. Records written before triggers
	// existed have none; see EffectiveTrigger.
	Trigger BackupTrigger `json:"trigger,omitempty"`

	// Database is the name of the backed up MongoDB database.
	Database string `json:"database"`

	// ConnectionID references the Connection the backup was taken from.
	ConnectionID string `json:"connection_id,omitempty"`

	// ConnectionName is a snapshot of the connection's name when the backup ran, kept
	// for display after the connection is renamed or deleted.
	ConnectionName string `json:"connection_name,omitempty"`

	// Status is the current lifecycle state of the backup.
	Status BackupStatus `json:"status"`

	// StorageType indicates whether stored in local disk or S3 object store.
	StorageType StorageType `json:"storage_type"`

	// StorageTargetID references the StorageTarget holding the artifact. Restores,
	// deletions and retention use this target, never the current default.
	StorageTargetID string `json:"storage_target_id,omitempty"`

	// StorageTargetName is a snapshot of the target's name when the backup ran.
	StorageTargetName string `json:"storage_target_name,omitempty"`

	// StorageKey is the relative path or object key in the storage backend.
	StorageKey string `json:"storage_key"`

	// SizeBytes is the total size of the stored artifact in bytes. For encrypted
	// backups this is the size of the age ciphertext, not of the plaintext archive.
	SizeBytes int64 `json:"size_bytes"`

	// SHA256 is the checksum of the stored bytes, calculated in-flight during streaming.
	// For encrypted backups it covers the age ciphertext, so it can be verified against
	// the storage object without any key material.
	SHA256 string `json:"sha256,omitempty"`

	// Encrypted reports whether the stored artifact is an age-encrypted stream.
	// Records written before encryption support decode as false (plaintext).
	Encrypted bool `json:"encrypted,omitempty"`

	// UsersAndRoles reports that the archive contains the users and roles defined on
	// Database (dumped with --dumpDbUsersAndRoles), so an in-place restore can
	// restore them (RestoreRequest.RestoreUsersAndRoles).
	UsersAndRoles bool `json:"users_and_roles,omitempty"`

	// EncryptionMode is the age recipient type used ("x25519" or "scrypt") when
	// Encrypted is true. It never contains key material.
	EncryptionMode string `json:"encryption_mode,omitempty"`

	// Collections lists specific collections included in the backup, if filtered.
	// Once the backup ran, wildcard patterns of the request are replaced by the
	// collections they matched.
	Collections []string `json:"collections,omitempty"`

	// ExcludedCollections lists the collections the backup excluded by request (with
	// wildcard patterns replaced by the collections they matched), if any. Exclusions
	// derived from an include filter are not listed.
	ExcludedCollections []string `json:"exclude_collections,omitempty"`

	// Filtered reports that the backup holds only some collections of its database
	// (Collections or ExcludedCollections is set). Such a backup still counts
	// towards the database's recovery point objective.
	Filtered bool `json:"filtered,omitempty"`

	// StartedAt is the timestamp when the dump process initiated.
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is the timestamp when the backup finalized and closed storage.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// DurationSeconds is the execution time in seconds.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`

	// ErrorMessage contains error details if Status is StatusFailed.
	ErrorMessage string `json:"error_message,omitempty"`

	// RetryOf is the ID of the failed backup this one retries, empty (omitted) for
	// backups that are not retries. The original record is never modified by a retry.
	RetryOf string `json:"retry_of,omitempty"`

	// CancelledBy names who cancelled the backup (a username, an API key or
	// "system") when Status is StatusCancelled.
	CancelledBy string `json:"cancelled_by,omitempty"`

	// CancelledAt is when the cancellation was requested.
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`

	// Phases holds the timestamps of the run's phases.
	Phases RunPhases `json:"phases,omitzero"`

	// Progress is the live progress of a running backup. It is filled in API
	// responses only and never stored.
	Progress *RunProgress `json:"progress,omitempty"`

	// VerifiedAt is when the stored archive was last re-read and compared with SHA256
	// (after upload, by an integrity sweep or on demand); nil if it never was.
	VerifiedAt *time.Time `json:"verified_at,omitempty"`

	// Verification is the outcome of that check; empty if it never ran.
	Verification VerificationStatus `json:"verification,omitempty"`

	// VerificationError explains a mismatch or an error (redacted).
	VerificationError string `json:"verification_error,omitempty"`

	// Manifest is captured during the backup (document counts and indexes per
	// collection). The store keeps it apart from the record, so record lists stay
	// small; it is never serialized with the record (see HasManifest).
	Manifest *Manifest `json:"-"`

	// HasManifest reports that a manifest was captured for this backup.
	HasManifest bool `json:"has_manifest,omitempty"`

	// ServerVersion is the MongoDB version (buildInfo) of the server the backup was
	// taken from, recorded with the manifest; empty for backups of earlier releases
	// and when it could not be read. Restore preflights compare it with the target.
	ServerVersion string `json:"server_version,omitempty"`

	// Pinned puts the backup on legal hold: retention never deletes it, and it can
	// only be deleted after it is unpinned.
	Pinned bool `json:"pinned,omitempty"`

	// PinNote is the optional reason given when pinning.
	PinNote string `json:"pin_note,omitempty"`

	// PinnedAt and PinnedBy record when and by whom the backup was pinned.
	PinnedAt *time.Time `json:"pinned_at,omitempty"`
	PinnedBy string     `json:"pinned_by,omitempty"`

	// Imported marks a record created from an orphan archive found by a storage scan.
	Imported bool `json:"imported,omitempty"`

	// ImportedAt is when it was imported.
	ImportedAt *time.Time `json:"imported_at,omitempty"`

	// MissingSince is when a storage scan first found the archive gone (StatusMissing).
	MissingSince *time.Time `json:"missing_since,omitempty"`

	// LastRestoreTest is the latest automated restore test of this backup.
	LastRestoreTest *RestoreTestSummary `json:"last_restore_test,omitempty"`

	// DeletedAt is when the backup was deleted (StatusDeleted and StatusPurged).
	DeletedAt *time.Time `json:"deleted_at,omitempty"`

	// DeletedBy names who deleted it: a username, "API key <name>", "retention" or
	// "system".
	DeletedBy string `json:"deleted_by,omitempty"`

	// DeleteApprovedBy names the second administrator who approved the deletion
	// (security.require_second_approver).
	DeleteApprovedBy string `json:"delete_approved_by,omitempty"`

	// DeleteReason is the optional reason given with the deletion (for retention,
	// the rule that selected it).
	DeleteReason string `json:"delete_reason,omitempty"`

	// PurgeAfter is the end of the grace period: the purge removes the archive once
	// it has passed. Until then the deletion can be undone.
	PurgeAfter *time.Time `json:"purge_after,omitempty"`

	// StatusBeforeDelete is the status the backup had when it was deleted; undeleting
	// restores it.
	StatusBeforeDelete BackupStatus `json:"status_before_delete,omitempty"`

	// PurgedAt is when the purge removed the archive (StatusPurged).
	PurgedAt *time.Time `json:"purged_at,omitempty"`
}

// SoftDelete describes a deletion: when, by whom and why, and the end of its grace
// period.
type SoftDelete struct {
	// At is the time of the deletion.
	At time.Time
	// PurgeAfter is the end of the grace period.
	PurgeAfter time.Time
	// By names who deleted the backup.
	By string
	// ApprovedBy names the second administrator who approved it, if any.
	ApprovedBy string
	// Reason is the optional reason.
	Reason string
}

// MarkDeleted moves r to StatusDeleted as d describes, remembering its current status
// for Undelete. The archive is not touched.
func (r *BackupRecord) MarkDeleted(d SoftDelete) {
	at, purge := d.At.UTC(), d.PurgeAfter.UTC()
	r.StatusBeforeDelete = r.Status
	r.Status = StatusDeleted
	r.DeletedAt, r.PurgeAfter = &at, &purge
	r.DeletedBy, r.DeleteApprovedBy, r.DeleteReason = d.By, d.ApprovedBy, d.Reason
	r.PurgedAt = nil
}

// Undelete restores the status r had before MarkDeleted (StatusCompleted for a record
// that did not remember it) and clears the deletion.
func (r *BackupRecord) Undelete() {
	prev := r.StatusBeforeDelete
	if prev == "" || prev.Deleted() {
		prev = StatusCompleted
	}
	r.Status = prev
	r.StatusBeforeDelete = ""
	r.DeletedAt, r.PurgeAfter, r.PurgedAt = nil, nil, nil
	r.DeletedBy, r.DeleteApprovedBy, r.DeleteReason = "", "", ""
}

// PurgeDue reports whether deleted record r may be purged at now: its grace period
// has ended, both as recorded (PurgeAfter) and as grace, the grace period in force
// now, counts it from DeletedAt (a longer grace period protects deletions made
// before it was raised). A pinned record, or one without a deletion time, is never
// due.
func (r *BackupRecord) PurgeDue(now time.Time, grace time.Duration) bool {
	if r.Status != StatusDeleted || r.Pinned || r.DeletedAt == nil || r.PurgeAfter == nil {
		return false
	}
	end := *r.PurgeAfter
	if byGrace := r.DeletedAt.Add(grace); byGrace.After(end) {
		end = byGrace
	}
	return !now.Before(end)
}

// EffectiveTrigger returns r.Trigger, or for records written before triggers existed
// TriggerScheduled when the record belongs to a job and TriggerManual otherwise (the
// same rule the schema migration backfills with).
func (r *BackupRecord) EffectiveTrigger() BackupTrigger {
	switch {
	case r.Trigger != "":
		return r.Trigger
	case r.JobID != "":
		return TriggerScheduled
	default:
		return TriggerManual
	}
}

// BackupOptions configures an on-demand or scheduled backup execution.
type BackupOptions struct {
	// Database is the target MongoDB database name to dump.
	Database string `json:"database"`

	// Collections optionally restricts the dump to a subset of collections.
	Collections []string `json:"collections,omitempty"`

	// ExcludeCollections lists collections to skip during the dump.
	ExcludeCollections []string `json:"exclude_collections,omitempty"`

	// Gzip specifies whether to compress the archive with gzip. Default is true.
	Gzip bool `json:"gzip"`

	// IncludeUsersAndRoles adds the users and roles defined on Database to the dump
	// (mongodump --dumpDbUsersAndRoles). It is ignored for the admin database, whose
	// users and roles are part of its own data. See UsersAndRolesApply.
	IncludeUsersAndRoles bool `json:"include_users_and_roles,omitempty"`

	// StorageType is the type of the destination target. It is filled from the
	// resolved target, never read from clients.
	StorageType StorageType `json:"-"`

	// StorageTargetID selects the StorageTarget to write to; empty means the default.
	StorageTargetID string `json:"storage_target_id,omitempty"`

	// StorageTargetName is the resolved target name recorded on the backup record.
	// It is filled by the server or scheduler, never read from clients.
	StorageTargetName string `json:"-"`

	// TargetKey is an optional custom path or key for the stored archive. It is set
	// by code only and never read from clients: a client-chosen key could overwrite
	// another backup's archive.
	TargetKey string `json:"-"`

	// ConnectionID selects the managed Connection to back up from (required by the API).
	ConnectionID string `json:"connection_id"`

	// ConnectionName is the resolved connection name recorded on the backup record.
	// It is filled by the server, never read from clients.
	ConnectionName string `json:"-"`

	// MongoURI is the resolved connection string of ConnectionID. It is filled by the
	// server or scheduler and never read from or written to JSON.
	MongoURI string `json:"-"`

	// JobID associates this run with a scheduled job.
	JobID string `json:"job_id,omitempty"`

	// Trigger is recorded on the backup record. It is set by the application (the
	// scheduler, the operations service), never read from clients.
	Trigger BackupTrigger `json:"-"`

	// Verify is the job's post-backup verification override (VerifyInherit for
	// manual backups). It is set by the application, never read from clients.
	Verify VerifyOverride `json:"-"`
}

// Redacted returns a copy of the options with the MongoURI password masked, suitable
// for logging or echoing back to API clients. Slice fields are cloned; the receiver
// is not modified.
func (o BackupOptions) Redacted() BackupOptions {
	o.Collections = slices.Clone(o.Collections)
	o.ExcludeCollections = slices.Clone(o.ExcludeCollections)
	o.MongoURI = redact.URI(o.MongoURI)
	return o
}

// Collection types of BackupCollection.Type.
const (
	// CollectionTypeCollection is a regular collection.
	CollectionTypeCollection = "collection"
	// CollectionTypeView is a view: it holds no data and is restored from its
	// definition, on top of its source collection (BackupCollection.ViewOn).
	CollectionTypeView = "view"
	// CollectionTypeTimeseries is a time-series collection.
	CollectionTypeTimeseries = "timeseries"
)

// BackupCollection is one collection stored in a backup archive, as listed by the
// archive contents preview (GET /api/v1/backups/{id}/collections).
type BackupCollection struct {
	// Name is the collection name.
	Name string `json:"name"`
	// Type is CollectionTypeCollection, CollectionTypeView or CollectionTypeTimeseries.
	Type string `json:"type"`
	// ViewOn is the source collection of a view.
	ViewOn string `json:"view_on,omitempty"`
	// SizeBytes is the data size recorded in the archive, when it has one.
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// AdminDatabase is MongoDB's admin database, which stores the users and roles of
// every database.
const AdminDatabase = "admin"

// UsersAndRolesApply reports whether a dump with these options includes the users and
// roles of its database: IncludeUsersAndRoles is set and the database is named and
// is not admin (mongodump needs --db for --dumpDbUsersAndRoles, and the admin
// database holds every user and role as regular data anyway).
func (o BackupOptions) UsersAndRolesApply() bool {
	db := strings.TrimSpace(o.Database)
	return o.IncludeUsersAndRoles && db != "" && db != AdminDatabase
}
