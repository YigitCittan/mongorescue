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

	// Database is the database of a single-database job (DatabaseSelection mode
	// single), kept for clients that predate database selections. It is empty for
	// jobs that cover several databases.
	Database string `json:"database"`

	// DatabaseSelection chooses the databases the job backs up. Jobs stored before
	// selections existed decode without one and back up Database (see Selection).
	DatabaseSelection DatabaseSelection `json:"database_selection"`

	// KnownDatabases are the databases an all or pattern selection matched when the
	// job was saved or last ran. Without AutoIncludeNew a run backs up only these
	// (plus the named ones) and reports the others as new. It is managed by the
	// server and never taken from clients.
	KnownDatabases []string `json:"known_databases,omitzero"`

	// Parallelism is how many databases a run backs up at the same time (1 to
	// MaxJobParallelism; 0 means 1). Every database keeps its own run lock.
	Parallelism int `json:"parallelism,omitempty"`

	// Collections optionally restricts the dump to specific collections. Only
	// single-database jobs use collection filters.
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

	// Enabled controls whether the scheduler actively triggers this job. A job with
	// Enabled false is paused.
	Enabled bool `json:"enabled"`

	// PausedUntil, set on a paused job, makes the scheduler resume it automatically at
	// that time. Nil means a paused job stays paused until it is resumed.
	PausedUntil *time.Time `json:"paused_until,omitempty"`

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

	// VerifyAfterBackup overrides the integrity.verify_after_backup setting for this
	// job's backups ("" follows the setting, "on" or "off").
	VerifyAfterBackup VerifyOverride `json:"verify_after_backup,omitempty"`

	// RestoreTest configures the automated restore test; nil means none.
	RestoreTest *RestoreTestPolicy `json:"restore_test,omitempty"`

	// LastRestoreTest is the latest restore test of the job. It is managed by the
	// server (like LastRun) and never taken from clients.
	LastRestoreTest *RestoreTestSummary `json:"last_restore_test,omitempty"`
}

// Clone returns a deep copy of the job.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	clone := *j
	clone.Collections = slices.Clone(j.Collections)
	clone.DatabaseSelection = j.DatabaseSelection.Clone()
	clone.KnownDatabases = slices.Clone(j.KnownDatabases)
	clone.ExcludeCollections = slices.Clone(j.ExcludeCollections)
	if j.PausedUntil != nil {
		until := *j.PausedUntil
		clone.PausedUntil = &until
	}
	if j.RestoreTest != nil {
		rt := *j.RestoreTest
		clone.RestoreTest = &rt
	}
	if j.LastRestoreTest != nil {
		lt := *j.LastRestoreTest
		clone.LastRestoreTest = &lt
	}
	return &clone
}

// Selection returns the job's database selection: DatabaseSelection, or for jobs
// stored without one a single selection of Database.
func (j *Job) Selection() DatabaseSelection {
	if j.DatabaseSelection.Mode == "" {
		sel := DatabaseSelection{Mode: SelectionSingle}
		if j.Database != "" {
			sel.Databases = []string{j.Database}
		}
		return sel
	}
	return j.DatabaseSelection.Clone()
}

// MultiDatabase reports whether the job's selection can cover several databases.
func (j *Job) MultiDatabase() bool {
	return j.Selection().Multi()
}

// EffectiveParallelism returns Parallelism within 1 and MaxJobParallelism.
func (j *Job) EffectiveParallelism() int {
	return min(max(j.Parallelism, 1), MaxJobParallelism)
}

// Covers reports whether the job backs up database name: its selection matches it
// (see DatabaseSelection.Matches) or it is one of the job's known databases.
func (j *Job) Covers(name string) bool {
	return name != "" && (j.Selection().Matches(name) || slices.Contains(j.KnownDatabases, name))
}

// SameSource reports whether j and o back up the same databases from the same
// connection: the same connection and selection (mode, databases, patterns and
// AutoIncludeNew).
func (j *Job) SameSource(o *Job) bool {
	a, b := j.Selection(), o.Selection()
	return j.ConnectionID == o.ConnectionID && a.SameMatch(b) && a.AutoIncludeNew == b.AutoIncludeNew
}
