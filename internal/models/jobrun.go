package models

import (
	"fmt"
	"time"
)

// JobRunStatus is the outcome of a job run over all its databases.
type JobRunStatus string

// Job run statuses.
const (
	// JobRunRunning is a run whose databases are still being backed up.
	JobRunRunning JobRunStatus = "running"
	// JobRunOK is a run whose every database was backed up.
	JobRunOK JobRunStatus = "ok"
	// JobRunPartial is a run in which some databases failed and others succeeded.
	JobRunPartial JobRunStatus = "partial"
	// JobRunFailed is a run in which no database was backed up.
	JobRunFailed JobRunStatus = "failed"
	// JobRunCancelled is a run that was stopped before any database failed; databases
	// finished before the cancellation keep their backups.
	JobRunCancelled JobRunStatus = "cancelled"
)

// ErrorDatabaseNotFound is the error recorded for a named database the server does
// not have.
const ErrorDatabaseNotFound = "database not found"

// JobRunDatabase is the outcome of one database of a job run.
type JobRunDatabase struct {
	// Database is the database name.
	Database string `json:"database"`
	// BackupID is the database's backup record; empty when no backup was started
	// (a database that does not exist).
	BackupID string `json:"backup_id,omitempty"`
	// Status is the backup's status (in_progress while it runs or waits).
	Status BackupStatus `json:"status"`
	// Error is the redacted failure reason.
	Error string `json:"error,omitempty"`
}

// JobRun groups the backups one run of a job took: one per database, each with its
// own record and archive, sharing the run's ID (BackupRecord.RunID).
type JobRun struct {
	// ID is the run's ID, also stored on its backup records as run_id.
	ID string `json:"id"`
	// JobID is the job.
	JobID string `json:"job_id"`
	// Trigger is how the run was started.
	Trigger BackupTrigger `json:"trigger"`
	// Status is the run's outcome (JobRunRunning while it runs).
	Status JobRunStatus `json:"status"`
	// StartedAt is when the run started.
	StartedAt time.Time `json:"started_at"`
	// CompletedAt is when its last database finished.
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// DurationSeconds is the run's duration.
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	// Databases lists the run's databases in the order they run.
	Databases []JobRunDatabase `json:"databases"`
	// NewDatabases lists databases that appeared since the job last ran and that the
	// run did not back up (the job does not include new databases automatically).
	NewDatabases []string `json:"new_databases,omitempty"`
	// AddedDatabases lists databases the run backed up for the first time because
	// the job includes new databases automatically.
	AddedDatabases []string `json:"added_databases,omitempty"`
	// Warnings are notes such as patterns that match no database.
	Warnings []string `json:"warnings,omitempty"`
	// Error is a failure of the run as a whole (such as a connection whose databases
	// cannot be listed), redacted.
	Error string `json:"error,omitempty"`
}

// Counts returns how many of the run's databases succeeded, failed, were cancelled
// and are still running.
func (r *JobRun) Counts() (succeeded, failed, cancelled, running int) {
	for _, d := range r.Databases {
		switch d.Status {
		case StatusCompleted:
			succeeded++
		case StatusCancelled:
			cancelled++
		case StatusInProgress, StatusPending:
			running++
		default:
			failed++
		}
	}
	return succeeded, failed, cancelled, running
}

// Finish sets the run's final status from its databases, its completion time and
// duration: ok when every database succeeded, partial when some failed and others
// succeeded, failed when none succeeded (or the run failed as a whole), cancelled
// when it was stopped and nothing failed.
func (r *JobRun) Finish(at time.Time) {
	ok, failed, cancelled, _ := r.Counts()
	switch {
	case r.Error != "" && ok == 0:
		r.Status = JobRunFailed
	case failed == 0 && cancelled == 0 && ok > 0:
		r.Status = JobRunOK
	case failed == 0 && cancelled > 0:
		r.Status = JobRunCancelled
	case ok > 0:
		r.Status = JobRunPartial
	default:
		r.Status = JobRunFailed
	}
	done := at.UTC()
	r.CompletedAt = &done
	if d := done.Sub(r.StartedAt).Seconds(); d > 0 {
		r.DurationSeconds = d
	}
}

// FailedDatabases returns the names of the run's failed databases.
func (r *JobRun) FailedDatabases() []string {
	var out []string
	for _, d := range r.Databases {
		switch d.Status {
		case StatusCompleted, StatusCancelled, StatusInProgress, StatusPending:
		default:
			out = append(out, d.Database)
		}
	}
	return out
}

// NewRunID returns a new job run ID ("run_<timestamp>_<suffix>").
func NewRunID(at time.Time) (string, error) {
	suffix, err := NewIDSuffix()
	if err != nil {
		return "", fmt.Errorf("new run id: %w", err)
	}
	return fmt.Sprintf("run_%s_%s", at.UTC().Format("20060102_150405"), suffix), nil
}
