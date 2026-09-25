package models

import (
	"slices"
	"time"
)

// Job represents a recurring scheduled backup task with retention policies.
type Job struct {
	// ID is the unique identifier for the job (e.g. "job_daily_analytics").
	ID string `json:"id"`

	// Name is a human-readable label for the job.
	Name string `json:"name"`

	// CronExpression specifies the schedule (e.g. "0 2 * * *" or "@daily", "@every 6h").
	CronExpression string `json:"cron_expression"`

	// Database is the target database name to back up.
	Database string `json:"database"`

	// Collections optionally restricts the dump to specific collections.
	Collections []string `json:"collections,omitempty"`

	// ExcludeCollections lists collections skipped by the dump.
	ExcludeCollections []string `json:"exclude_collections,omitempty"`

	// StorageType is the type of the job's storage target (local or s3), kept for
	// display; the target itself is StorageTargetID.
	StorageType StorageType `json:"storage_type"`

	// StorageTargetID references the StorageTarget the job writes to. It is set to
	// the default target when a job is saved without one.
	StorageTargetID string `json:"storage_target_id"`

	// RetentionDays specifies how many days to keep backups before pruning (0 = keep forever).
	RetentionDays int `json:"retention_days"`

	// RetentionCount specifies the maximum number of recent backups to keep (0 = unlimited).
	RetentionCount int `json:"retention_count"`

	// Gzip determines whether dumps are compressed. Default is true.
	Gzip bool `json:"gzip"`

	// Enabled controls whether the scheduler actively triggers this job.
	Enabled bool `json:"enabled"`

	// ConnectionID references the managed Connection the job backs up from.
	ConnectionID string `json:"connection_id"`

	// LastRun is the timestamp of the most recent execution.
	LastRun *time.Time `json:"last_run,omitempty"`

	// NextRun is the calculated timestamp of the upcoming execution.
	NextRun *time.Time `json:"next_run,omitempty"`

	// CreatedAt is when the job was configured.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the job was last modified.
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone returns a deep copy of the job.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	clone := *j
	clone.Collections = slices.Clone(j.Collections)
	clone.ExcludeCollections = slices.Clone(j.ExcludeCollections)
	return &clone
}
