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

	// IncludeUsersAndRoles makes every dump of the job include the users and roles
	// defined on its database (mongodump --dumpDbUsersAndRoles). The admin database is
	// dumped without the flag: its users and roles are part of its own data.
	IncludeUsersAndRoles bool `json:"include_users_and_roles"`

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

	// RPOMinutes is the job's recovery point objective: how old, in minutes, the
	// newest successful backup of each of its databases may be. 0 means unset: the
	// default from the schedule applies (see DefaultRPO). When set it is between
	// MinRPOMinutes and MaxRPOMinutes.
	RPOMinutes int `json:"rpo_minutes,omitempty"`

	// HeartbeatURL is an optional dead-man's-switch URL (healthchecks.io
	// compatible) pinged at "<url>/start" when a run starts, "<url>" when it
	// succeeds and "<url>/fail" when it fails or is partial; a cancelled or
	// interrupted run sends nothing after its start. Secret:
	// the store seals it and API responses show only its origin (see Redacted).
	HeartbeatURL string `json:"heartbeat_url,omitempty"`

	// ReadPreference overrides the read preference of the job's connection (the
	// member mongodump and the manifest capture read from); "" uses the
	// connection's. ReadPreferenceTags are its tag sets.
	ReadPreference     string              `json:"read_preference,omitempty"`
	ReadPreferenceTags []map[string]string `json:"read_preference_tags,omitempty"`

	// MaxUploadMbps caps the upload of every backup of the job, in megabits per
	// second; 0 uses the general.max_upload_mbps setting.
	MaxUploadMbps float64 `json:"max_upload_mbps,omitempty"`

	// NumParallelCollections is mongodump's --numParallelCollections (1 to
	// MaxNumParallelCollections); 0 keeps mongodump's default of 4.
	NumParallelCollections int `json:"num_parallel_collections,omitempty"`

	// BackupWindow, when set, restricts when scheduled runs may start (see
	// BackupWindow); manual runs ignore it.
	BackupWindow *BackupWindow `json:"backup_window,omitempty"`
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
	clone.ReadPreferenceTags = CloneTagSets(j.ReadPreferenceTags)
	clone.BackupWindow = j.BackupWindow.Clone()
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

// ReadPref returns the job's own read preference (zero when it uses its
// connection's).
func (j *Job) ReadPref() ReadPreference {
	return ReadPreference{Mode: j.ReadPreference, Tags: CloneTagSets(j.ReadPreferenceTags)}
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

// Bounds and defaults of a job's recovery point objective (Job.RPOMinutes).
const (
	// MinRPOMinutes is the smallest RPO a job may set (15 minutes).
	MinRPOMinutes = 15
	// MaxRPOMinutes is the largest RPO a job may set (90 days).
	MaxRPOMinutes = 90 * 24 * 60
	// DefaultRPOFloor is the smallest default RPO: a job without one never counts as
	// late within six hours of its last success.
	DefaultRPOFloor = 6 * time.Hour
	// DefaultRPOSlack is the slack the default RPO adds to two schedule intervals
	// (the largest gap between runs, see scheduler.Interval).
	DefaultRPOSlack = time.Hour
	// FallbackRPOInterval is the schedule interval assumed when a schedule cannot be
	// read (one day).
	FallbackRPOInterval = 24 * time.Hour
)

// DefaultRPO returns the RPO of a job without one, for a schedule whose runs are
// interval apart: two missed runs plus DefaultRPOSlack, never less than
// DefaultRPOFloor. A non-positive interval is read as FallbackRPOInterval.
func DefaultRPO(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = FallbackRPOInterval
	}
	return max(2*interval+DefaultRPOSlack, DefaultRPOFloor)
}

// RPO returns the job's recovery point objective: RPOMinutes when set, else
// DefaultRPO(interval), where interval is the gap between the job's scheduled runs.
// The second result reports whether the default applies.
func (j *Job) RPO(interval time.Duration) (time.Duration, bool) {
	if j.RPOMinutes > 0 {
		return time.Duration(j.RPOMinutes) * time.Minute, false
	}
	return DefaultRPO(interval), true
}

// RPOBreach records that a job's recovery point objective is missed for one of its
// databases: Since is when the breach was first seen.
type RPOBreach struct {
	// JobID is the job.
	JobID string `json:"job_id"`
	// Database is the database whose newest successful backup is too old.
	Database string `json:"database"`
	// Since is when the breach was first seen (UTC).
	Since time.Time `json:"since"`
}
