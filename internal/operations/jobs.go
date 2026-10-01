package operations

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// DefaultCronExpression is the schedule of a job saved without one.
const DefaultCronExpression = "@daily"

// JobDetailsNextRuns is how many upcoming activations GetJobDetails reports.
const JobDetailsNextRuns = 3

// Job validation errors. They are ErrInvalid errors: adapters answer them with 400.
var (
	// ErrDatabaseRequired is returned when a job names no database.
	ErrDatabaseRequired = errors.New("database is required")
	// ErrNegativeRetention is returned when retention_days or retention_count is
	// negative.
	ErrNegativeRetention = errors.New("retention_days and retention_count must not be negative")
	// ErrPausedUntilPast is returned when paused_until is not in the future.
	ErrPausedUntilPast = errors.New("paused_until must be in the future")
)

// ErrJobChanged is returned by UpdateJob when the request names the job's updated_at
// and the job was changed since (adapters answer 409 Conflict).
var ErrJobChanged = errors.New("the job was changed meanwhile; reload it and try again")

// JobScheduler (re)schedules jobs in the running scheduler (implemented by
// *scheduler.Scheduler).
type JobScheduler interface {
	// ApplyJobUpdate runs persist (which stores job) and then replaces the job's cron
	// entry, under one lock, so concurrent updates are scheduled in the order they
	// were stored. It returns persist's error unchanged.
	ApplyJobUpdate(job *models.Job, persist func() error) error
}

// JobUpdate is the body of PUT /api/v1/jobs/{id}. The schedule, database, collection
// filters, connection and storage target are replaced; omitted retention, gzip and
// enabled fields keep the job's current values. The id, creation time and run history
// never change.
type JobUpdate struct {
	// Name is the human-readable label.
	Name string `json:"name"`
	// CronExpression is the schedule; "" means DefaultCronExpression.
	CronExpression string `json:"cron_expression"`
	// Database is the database to back up.
	Database string `json:"database"`
	// Collections restricts the dump to these collections.
	Collections []string `json:"collections"`
	// ExcludeCollections lists collections skipped by the dump.
	ExcludeCollections []string `json:"exclude_collections"`
	// ConnectionID names the managed connection to back up from.
	ConnectionID string `json:"connection_id"`
	// StorageTargetID names the storage target; "" means the default target.
	StorageTargetID string `json:"storage_target_id"`
	// RetentionDays keeps backups for this many days (0 = forever) when set.
	RetentionDays *int `json:"retention_days"`
	// RetentionCount keeps at most this many backups (0 = unlimited) when set.
	RetentionCount *int `json:"retention_count"`
	// Gzip compresses the dumps when set.
	Gzip *bool `json:"gzip"`
	// Enabled schedules (true) or pauses (false) the job when set.
	Enabled *bool `json:"enabled"`
	// PausedUntil, for a paused job, resumes it automatically at that time. Pausing
	// without it pauses until resumed; a job left paused keeps its current value.
	PausedUntil *time.Time `json:"paused_until"`
	// UpdatedAt, when set, is the job's updated_at the client edited: the update is
	// refused with ErrJobChanged if the job was changed since.
	UpdatedAt *time.Time `json:"updated_at"`
}

// JobDetails is a job with its upcoming activations.
type JobDetails struct {
	*models.Job
	// NextRuns lists the next JobDetailsNextRuns activations in UTC; it is empty for
	// a disabled job or an unparsable schedule.
	NextRuns []time.Time `json:"next_runs"`
}

// ValidateJob checks and normalises a job before it is created or updated: an empty
// schedule becomes DefaultCronExpression, the schedule must parse, the database must be
// named, retention must not be negative, the connection must exist and the storage
// target (the default one for "") is resolved into StorageTargetID and StorageType.
// Expected failures: ErrInvalid (wrapping scheduler.ErrInvalidCron,
// ErrDatabaseRequired or ErrNegativeRetention), ErrConnectionRequired,
// ErrUnknownConnection and ErrUnknownStorageTarget. An enabled job's PausedUntil is
// cleared; a stored PausedUntil that has passed is kept (the scheduler resumes the
// job within a minute), so it never blocks an edit. A PausedUntil sent by a client is
// checked with ValidatePausedUntil.
func (s *Service) ValidateJob(ctx context.Context, job *models.Job) error {
	job.CronExpression = strings.TrimSpace(job.CronExpression)
	if job.CronExpression == "" {
		job.CronExpression = DefaultCronExpression
	}
	if err := scheduler.ValidateCron(job.CronExpression); err != nil {
		return invalid(err)
	}
	if strings.TrimSpace(job.Database) == "" {
		return invalid(ErrDatabaseRequired)
	}
	if err := validateNamespaces(job.Database, job.Collections, job.ExcludeCollections); err != nil {
		return err
	}
	if job.RetentionDays < 0 || job.RetentionCount < 0 {
		return invalid(ErrNegativeRetention)
	}
	if job.Enabled {
		job.PausedUntil = nil
	}
	if job.PausedUntil != nil {
		until := job.PausedUntil.UTC()
		job.PausedUntil = &until
	}
	if _, err := s.ResolveConnection(ctx, job.ConnectionID); err != nil {
		return err
	}
	target, err := s.ResolveTarget(ctx, job.StorageTargetID)
	if err != nil {
		return err
	}
	job.StorageTargetID, job.StorageType = target.ID, target.Type
	return nil
}

// ValidatePausedUntil checks a paused_until sent by a client: it must be in the
// future (ErrPausedUntilPast, an ErrInvalid error). Nil is valid.
func (s *Service) ValidatePausedUntil(until *time.Time) error {
	if until != nil && !until.After(s.now()) {
		return invalid(ErrPausedUntilPast)
	}
	return nil
}

// UpdateJob replaces the editable fields of job id with u, validates the result like
// a new job (see ValidateJob), stores it and reschedules it at once: the new schedule,
// or a pause, takes effect without a restart. The job keeps its id, creation time,
// last run and backups. Expected failures: ErrNotFound, ErrJobChanged,
// ErrPausedUntilPast (for a paused_until sent in the past) and those of ValidateJob.
func (s *Service) UpdateJob(ctx context.Context, id string, u JobUpdate) (*models.Job, error) {
	existing, err := s.cfg.Store.GetJob(ctx, id)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	job := existing.Clone()
	job.Name = u.Name
	job.CronExpression = u.CronExpression
	job.Database = u.Database
	job.Collections = u.Collections
	job.ExcludeCollections = u.ExcludeCollections
	job.ConnectionID = u.ConnectionID
	job.StorageTargetID = u.StorageTargetID
	job.RetentionDays = derefOr(u.RetentionDays, existing.RetentionDays)
	job.RetentionCount = derefOr(u.RetentionCount, existing.RetentionCount)
	job.Gzip = derefOr(u.Gzip, existing.Gzip)
	job.Enabled = derefOr(u.Enabled, existing.Enabled)
	switch {
	case job.Enabled:
		job.PausedUntil = nil
	case u.PausedUntil != nil:
		if err = s.ValidatePausedUntil(u.PausedUntil); err != nil {
			return nil, err
		}
		job.PausedUntil = u.PausedUntil
	case existing.Enabled:
		// Paused now without a time: until resumed.
		job.PausedUntil = nil
	}
	if err = s.ValidateJob(ctx, job); err != nil {
		return nil, err
	}
	// The next run belongs to the old schedule until the scheduler computes the new one.
	job.NextRun = nil
	if job.Enabled && s.cfg.Scheduler == nil {
		job.NextRun = nextRunOf(job.CronExpression, s.now())
	}
	// persist re-reads the job under the scheduler's lock: the precondition is checked
	// against, and the run history taken from, what is stored right now, so a run that
	// finished since the first read is not reverted.
	persist := func() error {
		current, getErr := s.cfg.Store.GetJob(ctx, id)
		if getErr != nil {
			return notFound(getErr, "job not found")
		}
		if u.UpdatedAt != nil && !current.UpdatedAt.Equal(*u.UpdatedAt) {
			return ErrJobChanged
		}
		job.LastRun, job.CreatedAt = current.LastRun, current.CreatedAt
		// UpdateJob never recreates a job deleted meanwhile.
		return notFound(s.cfg.Store.UpdateJob(ctx, job), "job not found")
	}
	if s.cfg.Scheduler != nil {
		err = s.cfg.Scheduler.ApplyJobUpdate(job, persist)
	} else {
		err = persist()
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// GetJobDetails returns job id with its next JobDetailsNextRuns activations, or an
// ErrNotFound error.
func (s *Service) GetJobDetails(ctx context.Context, id string) (*JobDetails, error) {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	details := &JobDetails{Job: job, NextRuns: []time.Time{}}
	if job.Enabled {
		if runs := scheduler.NextRuns(job.CronExpression, s.now(), JobDetailsNextRuns); runs != nil {
			details.NextRuns = runs
		}
	}
	return details, nil
}

// nextRunOf returns the first activation of expr after from, or nil.
func nextRunOf(expr string, from time.Time) *time.Time {
	runs := scheduler.NextRuns(expr, from, 1)
	if len(runs) == 0 {
		return nil
	}
	return &runs[0]
}
