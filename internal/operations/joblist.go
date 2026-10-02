package operations

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Schedule frequencies of the jobs list filter (BulkFilter.Schedule), derived from a
// job's cron expression by ScheduleFrequency.
const (
	// FrequencyHourly is a schedule that runs more than once a day.
	FrequencyHourly = "hourly"
	// FrequencyDaily is a schedule that runs once every day.
	FrequencyDaily = "daily"
	// FrequencyWeekly is a schedule that runs on some days of the week.
	FrequencyWeekly = "weekly"
	// FrequencyMonthly is a schedule that runs on some days of the month.
	FrequencyMonthly = "monthly"
	// FrequencyOther is any other schedule (yearly, some months, unusual intervals).
	FrequencyOther = "other"
)

// Last-run states of the jobs list filter (BulkFilter.LastStatus): the state of a
// job's newest backup.
const (
	// LastRunCompleted is a job whose newest backup completed (also when it was
	// pruned or its archive went missing since).
	LastRunCompleted = "completed"
	// LastRunFailed is a job whose newest backup failed (a multi-database job: whose
	// last run backed up no database).
	LastRunFailed = "failed"
	// LastRunPartial is a multi-database job whose last run backed up some of its
	// databases and failed on others.
	LastRunPartial = "partial"
	// LastRunInProgress is a job whose newest backup is pending or running.
	LastRunInProgress = "in_progress"
	// LastRunCancelled is a job whose newest backup was cancelled.
	LastRunCancelled = "cancelled"
	// LastRunNever is a job without backups.
	LastRunNever = "never"
)

// JobFrequencies and JobLastStatuses are the accepted values of BulkFilter.Schedule
// and BulkFilter.LastStatus.
var (
	JobFrequencies  = []string{FrequencyHourly, FrequencyDaily, FrequencyWeekly, FrequencyMonthly, FrequencyOther}
	JobLastStatuses = []string{LastRunCompleted, LastRunFailed, LastRunPartial, LastRunInProgress, LastRunCancelled, LastRunNever}
)

// ScheduleFrequency classifies a cron expression (five fields or a descriptor such as
// "@daily" or "@every 6h", with an optional time zone prefix) as one of
// JobFrequencies. Expressions it cannot read are FrequencyOther.
func ScheduleFrequency(expr string) string {
	s := strings.TrimSpace(expr)
	if strings.HasPrefix(s, "TZ=") || strings.HasPrefix(s, "CRON_TZ=") {
		_, rest, ok := strings.Cut(s, " ")
		if !ok {
			return FrequencyOther
		}
		s = strings.TrimSpace(rest)
	}
	switch s {
	case "@hourly":
		return FrequencyHourly
	case "@daily", "@midnight":
		return FrequencyDaily
	case "@weekly":
		return FrequencyWeekly
	case "@monthly":
		return FrequencyMonthly
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		return everyFrequency(strings.TrimSpace(rest))
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return FrequencyOther
	}
	minute, hour, dom, month, dow := f[0], f[1], f[2], f[3], f[4]
	every := func(v string) bool { return v == "*" || v == "?" }
	switch {
	case !every(month):
		return FrequencyOther
	case !singleValue(minute) || !singleValue(hour):
		return FrequencyHourly
	case every(dom) && every(dow):
		return FrequencyDaily
	case every(dom):
		return FrequencyWeekly
	case every(dow):
		return FrequencyMonthly
	}
	return FrequencyOther
}

// everyFrequency classifies the interval of an "@every" descriptor.
func everyFrequency(interval string) string {
	d, err := time.ParseDuration(interval)
	switch {
	case err != nil || d <= 0:
		return FrequencyOther
	case d < 24*time.Hour:
		return FrequencyHourly
	case d == 24*time.Hour:
		return FrequencyDaily
	case d == 7*24*time.Hour:
		return FrequencyWeekly
	}
	return FrequencyOther
}

// singleValue reports whether a cron field is one number (no list, range or step).
func singleValue(v string) bool {
	if v == "" || len(v) > 2 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
	backups, err := s.cfg.Store.LatestJobBackups(ctx, "")
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
		if runs, err = s.cfg.Store.LatestJobRuns(ctx, multi); err != nil {
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
	if err := checkText(map[string]string{"database": f.Database, "connection_id": f.ConnectionID, "q": f.Q}); err != nil {
		return nil, err
	}
	if f.Schedule != "" && !slices.Contains(JobFrequencies, f.Schedule) {
		return nil, public("schedule must be one of "+strings.Join(JobFrequencies, ", "), ErrInvalid)
	}
	if f.LastStatus != "" && !slices.Contains(JobLastStatuses, f.LastStatus) {
		return nil, public("last_status must be one of "+strings.Join(JobLastStatuses, ", "), ErrInvalid)
	}
	jobs, err := s.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
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
