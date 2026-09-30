package operations

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
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
)

// JobScheduler (re)schedules jobs in the running scheduler (implemented by
// *scheduler.Scheduler).
type JobScheduler interface {
	// RegisterJob replaces the cron entry of job: a disabled job is unscheduled, an
	// enabled one is scheduled with its current expression and its next run stored.
	RegisterJob(job *models.Job) error
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
// ErrUnknownConnection and ErrUnknownStorageTarget.
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
	if job.RetentionDays < 0 || job.RetentionCount < 0 {
		return invalid(ErrNegativeRetention)
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

// UpdateJob replaces the editable fields of job id with u, validates the result like
// a new job (see ValidateJob), stores it and reschedules it at once: the new schedule,
// or a pause, takes effect without a restart. The job keeps its id, creation time,
// last run and backups. Expected failures: ErrNotFound and those of ValidateJob.
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
	if err = s.ValidateJob(ctx, job); err != nil {
		return nil, err
	}
	// The next run belongs to the old schedule until the scheduler computes the new one.
	job.NextRun = nil
	if job.Enabled && s.cfg.Scheduler == nil {
		job.NextRun = nextRunOf(job.CronExpression, s.now())
	}
	// UpdateJob never recreates a job deleted meanwhile.
	if err = s.cfg.Store.UpdateJob(ctx, job); err != nil {
		return nil, notFound(err, "job not found")
	}
	if s.cfg.Scheduler != nil {
		if regErr := s.cfg.Scheduler.RegisterJob(job); regErr != nil {
			s.logger.Error("failed to reschedule job", slog.String("job_id", job.ID), logsafe.Error(regErr))
		}
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
