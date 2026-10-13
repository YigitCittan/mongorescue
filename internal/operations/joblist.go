package operations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Schedule frequencies of the jobs list filter (BulkFilter.Schedule), derived from a
// job's cron expression by ScheduleFrequency.
const (
	// FrequencyHourly is a schedule that runs more than once a day.
	FrequencyHourly = models.FrequencyHourly
	// FrequencyDaily is a schedule that runs once every day.
	FrequencyDaily = models.FrequencyDaily
	// FrequencyWeekly is a schedule that runs on some days of the week.
	FrequencyWeekly = models.FrequencyWeekly
	// FrequencyMonthly is a schedule that runs on some days of the month.
	FrequencyMonthly = models.FrequencyMonthly
	// FrequencyOther is any other schedule (yearly, some months, unusual intervals).
	FrequencyOther = models.FrequencyOther
)

// Last-run states of the jobs list filter (BulkFilter.LastStatus): the state of a
// job's newest backup.
const (
	// LastRunCompleted is a job whose newest backup completed (also when it was
	// pruned or its archive went missing since).
	LastRunCompleted = store.JobLastCompleted
	// LastRunFailed is a job whose newest backup failed (a multi-database job: whose
	// last run backed up no database).
	LastRunFailed = store.JobLastFailed
	// LastRunPartial is a multi-database job whose last run backed up some of its
	// databases and failed on others.
	LastRunPartial = store.JobLastPartial
	// LastRunInProgress is a job whose newest backup is pending or running.
	LastRunInProgress = store.JobLastInProgress
	// LastRunCancelled is a job whose newest backup was cancelled.
	LastRunCancelled = store.JobLastCancelled
	// LastRunNever is a job without backups.
	LastRunNever = store.JobLastNever
)

// JobFrequencies and JobLastStatuses are the accepted values of BulkFilter.Schedule
// and BulkFilter.LastStatus.
var (
	JobFrequencies  = []string{FrequencyHourly, FrequencyDaily, FrequencyWeekly, FrequencyMonthly, FrequencyOther}
	JobLastStatuses = []string{LastRunCompleted, LastRunFailed, LastRunPartial, LastRunInProgress, LastRunCancelled, LastRunNever}
)

// ScheduleFrequency classifies a cron expression (five fields or a descriptor such as
// "@daily" or "@every 6h", with an optional time zone prefix) as one of
// JobFrequencies. Expressions it cannot read are FrequencyOther. It is
// models.ScheduleFrequency, which the store also uses for its schedule column.
func ScheduleFrequency(expr string) string {
	return models.ScheduleFrequency(expr)
}

// lastRunStatus is the BulkFilter.LastStatus value of a job whose newest backup is b
// (nil without backups).
func lastRunStatus(b *models.BackupRecord) string {
	if b == nil {
		return LastRunNever
	}
	switch b.Status {
	case models.StatusFailed:
		return LastRunFailed
	case models.StatusPending, models.StatusInProgress:
		return LastRunInProgress
	case models.StatusCancelled:
		return LastRunCancelled
	}
	return LastRunCompleted
}

// runLastStatus is the BulkFilter.LastStatus value of a multi-database job whose
// newest run is r (nil without runs): the run's outcome, never one database's backup,
// as the dashboard shows it.
func runLastStatus(r *models.JobRun) string {
	if r == nil {
		return LastRunNever
	}
	switch r.Status {
	case models.JobRunOK:
		return LastRunCompleted
	case models.JobRunPartial:
		return LastRunPartial
	case models.JobRunFailed:
		return LastRunFailed
	case models.JobRunCancelled:
		return LastRunCancelled
	}
	return LastRunInProgress
}

// jobLastStatus is the BulkFilter.LastStatus value of j; lastBackups maps job IDs to
// their newest backup, lastRuns multi-database job IDs to their newest run.
func jobLastStatus(j *models.Job, lastBackups map[string]*models.BackupRecord, lastRuns map[string]*models.JobRun) string {
	if !j.MultiDatabase() {
		return lastRunStatus(lastBackups[j.ID])
	}
	return runLastStatus(lastRuns[j.ID])
}

// lastOutcomes loads, in one query each, the newest backup of every job and the
// newest run of the multi-database jobs among jobs (never one query per job).
func (s *Service) lastOutcomes(ctx context.Context, jobs []*models.Job) (map[string]*models.BackupRecord, map[string]*models.JobRun, error) {
	backups, err := s.store.LatestJobBackups(ctx, "")
	if err != nil {
		return nil, nil, fmt.Errorf("load the jobs' last backups: %w", err)
	}
	var multi []string
	for _, j := range jobs {
		if j.MultiDatabase() {
			multi = append(multi, j.ID)
		}
	}
	runs := map[string]*models.JobRun{}
	if len(multi) > 0 {
		if runs, err = s.store.LatestJobRuns(ctx, multi); err != nil {
			return nil, nil, fmt.Errorf("load the jobs' last runs: %w", err)
		}
	}
	return backups, runs, nil
}

// FilterJobs returns the jobs matching f, sorted by name: the filter of
// GET /api/v1/jobs, shared with the bulk job actions (the same matcher, so "select
// all matching" selects exactly the listed jobs). Only the job fields of BulkFilter
// apply (database, connection_id, q, enabled, schedule, last_status); other fields,
// unknown values and over-long texts return ErrInvalid errors.
func (s *Service) FilterJobs(ctx context.Context, f BulkFilter) ([]*models.Job, error) {
	if err := checkBulkFilter(BulkJobs, &f); err != nil {
		return nil, err
	}
	return s.matchingJobs(ctx, &f)
}

// matchingJobs validates the job filter values of f and returns the matching jobs,
// sorted by name.
func (s *Service) matchingJobs(ctx context.Context, f *BulkFilter) ([]*models.Job, error) {
	if err := checkJobFilter(f); err != nil {
		return nil, err
	}
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	return s.keepMatchingJobs(ctx, jobs, f)
}

// checkJobFilter validates the job filter values of f.
func checkJobFilter(f *BulkFilter) error {
	if err := checkText(map[string]string{"database": f.Database, "connection_id": f.ConnectionID, "q": f.Q}); err != nil {
		return err
	}
	if f.Schedule != "" && !slices.Contains(JobFrequencies, f.Schedule) {
		return public("schedule must be one of "+strings.Join(JobFrequencies, ", "), ErrInvalid)
	}
	if f.LastStatus != "" && !slices.Contains(JobLastStatuses, f.LastStatus) {
		return public("last_status must be one of "+strings.Join(JobLastStatuses, ", "), ErrInvalid)
	}
	return nil
}

// keepMatchingJobs returns the jobs of jobs matching f, in order.
func (s *Service) keepMatchingJobs(ctx context.Context, jobs []*models.Job, f *BulkFilter) ([]*models.Job, error) {
	q := strings.ToLower(f.Q)
	out := make([]*models.Job, 0, len(jobs))
	for _, j := range jobs {
		if jobMatches(j, f, q) {
			out = append(out, j)
		}
	}
	if f.LastStatus == "" || len(out) == 0 {
		return out, nil
	}
	lastBackups, lastRuns, err := s.lastOutcomes(ctx, out)
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, j := range out {
		if jobLastStatus(j, lastBackups, lastRuns) == f.LastStatus {
			kept = append(kept, j)
		}
	}
	return kept, nil
}
