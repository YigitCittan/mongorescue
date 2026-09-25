package models

import (
	"slices"
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
)

// BackupRecord represents a persistent record of a completed or running backup.
type BackupRecord struct {
	// ID is the unique identifier for this backup (e.g. "bkp_20260924_153000_mydb").
	ID string `json:"id"`

	// JobID references the scheduled job that triggered this backup, if any.
	JobID string `json:"job_id,omitempty"`

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

	// EncryptionMode is the age recipient type used ("x25519" or "scrypt") when
	// Encrypted is true. It never contains key material.
	EncryptionMode string `json:"encryption_mode,omitempty"`

	// Collections lists specific collections included in the backup, if filtered.
	Collections []string `json:"collections,omitempty"`

	// StartedAt is the timestamp when the dump process initiated.
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is the timestamp when the backup finalized and closed storage.
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// DurationSeconds is the execution time in seconds.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`

	// ErrorMessage contains error details if Status is StatusFailed.
	ErrorMessage string `json:"error_message,omitempty"`
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

	// StorageType is the type of the destination target. It is filled from the
	// resolved target, never read from clients.
	StorageType StorageType `json:"-"`

	// StorageTargetID selects the StorageTarget to write to; empty means the default.
	StorageTargetID string `json:"storage_target_id,omitempty"`

	// StorageTargetName is the resolved target name recorded on the backup record.
	// It is filled by the server or scheduler, never read from clients.
	StorageTargetName string `json:"-"`

	// TargetKey is an optional custom path or key for the stored archive.
	TargetKey string `json:"target_key,omitempty"`

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
