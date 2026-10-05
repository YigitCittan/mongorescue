package operations

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	// The IANA zone database, so callers' zones resolve also where the system has
	// none (Windows, minimal containers).
	_ "time/tzdata"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Limits of History and SchedulePreview.
const (
	// DefaultHistoryDays is the number of days History covers when none is given.
	DefaultHistoryDays = 30
	// MaxHistoryDays is the most days History covers.
	MaxHistoryDays = store.MaxHistoryDays
	// MaxHistoryOffsetMinutes bounds the time zone offset of History's days.
	MaxHistoryOffsetMinutes = 14 * 60
	// HistoryRunsPerJob is how many of each job's newest backups History reports.
	HistoryRunsPerJob = 12
	// HistoryMaxVerificationIssues bounds the failed verifications History lists.
	HistoryMaxVerificationIssues = 20
	// UpcomingWindow is how far ahead History lists the scheduled runs.
	UpcomingWindow = 24 * time.Hour
	// MaxUpcomingPerJob and MaxUpcoming bound those runs (per job and in total).
	MaxUpcomingPerJob = 96
	MaxUpcoming       = 500
	// MaxSchedulePreviewRuns is the most activations SchedulePreview returns.
	MaxSchedulePreviewRuns = 10
	// maxCronLength bounds a previewed cron expression.
	maxCronLength = 256
)

// HistoryRequest selects the window of History.
type HistoryRequest struct {
	// Days is the number of days up to and including today (1 to MaxHistoryDays; 0
	// means DefaultHistoryDays).
	Days int
	// TimeZone is the caller's IANA time zone (e.g. "Europe/Istanbul"): days start
	// at its local midnights, also across daylight saving changes. An empty or
	// unknown zone falls back to OffsetMinutes.
	TimeZone string
	// OffsetMinutes is the caller's UTC offset, east positive (e.g. 180 for UTC+3),
	// used as a fixed zone when TimeZone is empty or unknown.
	OffsetMinutes int
}

// maxTimeZoneName bounds HistoryRequest.TimeZone (IANA names are far shorter).
const maxTimeZoneName = 64

// location returns the zone the days of req are cut in and its name: TimeZone when
// the server knows it, else the fixed OffsetMinutes zone ("UTC+03:00").
func (req HistoryRequest) location() (*time.Location, string) {
	name := req.TimeZone
	// "Local" would be the server's zone, not the caller's; "" is UTC to LoadLocation.
	if name != "" && name != "Local" && len(name) <= maxTimeZoneName {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc, name
		}
	}
	sign, off := "+", req.OffsetMinutes
	if off < 0 {
		sign, off = "-", -off
	}
	label := fmt.Sprintf("UTC%s%02d:%02d", sign, off/60, off%60)
	return time.FixedZone(label, req.OffsetMinutes*60), label
}

// HistoryDay is one day of History.
type HistoryDay struct {
	// Date is the day in the caller's time zone (YYYY-MM-DD).
	Date string `json:"date"`
	// Completed, Failed and Cancelled count the backups that started that day.
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// Bytes sums the size of that day's completed backups still on record.
	Bytes int64 `json:"bytes"`
	// StoredBytes is the size of the completed backups on record that started up to
	// the end of the day: the storage curve of the archives kept today.
	StoredBytes int64 `json:"stored_bytes"`
}

// HistoryRun is one of a job's recent backups.
type HistoryRun struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// Status is its state.
	Status models.BackupStatus `json:"status"`
	// StartedAt is when it started.
	StartedAt time.Time `json:"started_at"`
	// DurationSeconds is how long it ran (0 while running or unknown).
	DurationSeconds float64 `json:"duration_seconds"`
	// RunStatus is the outcome of a multi-database job's run (ok, partial, failed,
	// cancelled, running): such a job lists its runs, each as one entry whose ID is
	// the run's, not one per database. Status is then completed for ok, failed for
	// partial and failed runs, cancelled, or in_progress.
	RunStatus models.JobRunStatus `json:"run_status,omitempty"`
	// Databases and Succeeded count a run's databases and those backed up.
	Databases int `json:"databases,omitempty"`
	Succeeded int `json:"succeeded,omitempty"`
	// FailedDatabases names a run's failed databases.
	FailedDatabases []string `json:"failed_databases,omitempty"`
}

// HistoryJob is the recent record of one job.
type HistoryJob struct {
	// Runs are its newest backups (at most HistoryRunsPerJob), oldest first.
	Runs []HistoryRun `json:"runs"`
	// LastSuccessAt is when its newest completed backup started, if any. For a job
	// with several databases it is the oldest of its databases' newest successes
	// (the stalest database), empty while one of them never succeeded.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// StalestDatabase names the database LastSuccessAt belongs to, or the first one
	// that never succeeded (multi-database jobs).
	StalestDatabase string `json:"stalest_database,omitempty"`
	// Databases breaks the last successes down per database (multi-database jobs).
	Databases []DatabaseStatus `json:"databases,omitempty"`
	// IntervalSeconds is the gap between the enabled job's next two scheduled runs
	// (0 for disabled jobs), so "no success for too long" can follow the schedule.
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
}

// UpcomingRun is a scheduled activation of a job.
type UpcomingRun struct {
	// JobID is the job.
	JobID string `json:"job_id"`
	// At is when it runs (UTC).
	At time.Time `json:"at"`
}

// VerificationIssue is a completed backup whose verification failed.
type VerificationIssue struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// JobID is its job, if any.
	JobID string `json:"job_id,omitempty"`
	// Database is the backed-up database.
	Database string `json:"database"`
	// StartedAt is when the backup started.
	StartedAt time.Time `json:"started_at"`
	// Verification is "mismatch" or "error".
	Verification models.VerificationStatus `json:"verification"`
}

// ServerTimeZone describes the time zone cron expressions are evaluated in.
type ServerTimeZone struct {
	// Name is the zone's name (e.g. "Europe/Istanbul", or an abbreviation such as
	// "UTC" when the process only knows its local zone).
	Name string `json:"name"`
	// OffsetMinutes is its current UTC offset, east positive.
	OffsetMinutes int `json:"offset_minutes"`
}

// History is the dashboard's overview over time. Everything comes from SQL aggregates
// and the job list; no backup record is read in full.
type History struct {
	// Days is the number of days covered.
	Days int `json:"days"`
	// TimeZone is the zone the days were cut in: the caller's IANA zone, or the
	// fixed offset ("UTC+03:00") it fell back to.
	TimeZone string `json:"time_zone"`
	// OffsetMinutes is that zone's UTC offset now.
	OffsetMinutes int `json:"offset_minutes"`
	// From is the start of the first day (UTC).
	From time.Time `json:"from"`
	// GeneratedAt is when History was computed (UTC).
	GeneratedAt time.Time `json:"generated_at"`
	// Daily lists every day of the window, oldest first, including empty ones.
	Daily []HistoryDay `json:"daily"`
	// StoredBytesBefore sums the completed backups on record that started earlier.
	StoredBytesBefore int64 `json:"stored_bytes_before"`
	// Jobs maps each job ID with backups or an enabled schedule to its recent runs,
	// last success and run interval.
	Jobs map[string]HistoryJob `json:"jobs"`
	// Upcoming lists the enabled jobs' activations in the next UpcomingWindow, in
	// order; UpcomingTruncated reports that the caps cut the list.
	Upcoming          []UpcomingRun `json:"upcoming"`
	UpcomingTruncated bool          `json:"upcoming_truncated,omitempty"`
	// VerificationIssues lists the newest completed backups whose verification
	// failed; VerificationIssuesTotal counts all of them.
	VerificationIssues      []VerificationIssue `json:"verification_issues"`
	VerificationIssuesTotal int                 `json:"verification_issues_total"`
	// ServerTimeZone is the zone cron expressions use.
	ServerTimeZone ServerTimeZone `json:"server_time_zone"`
}

// History returns the outcomes and stored sizes per day, the jobs' recent runs, the
// next day's scheduled runs and the failed verifications. Days or offsets out of
// range return an ErrInvalid error.
func (s *Service) History(ctx context.Context, req HistoryRequest) (*History, error) {
	days := req.Days
	if days == 0 {
		days = DefaultHistoryDays
	}
	if days < 1 || days > MaxHistoryDays {
		return nil, public(fmt.Sprintf("days must be between 1 and %d", MaxHistoryDays), ErrInvalid)
	}
	if req.OffsetMinutes < -MaxHistoryOffsetMinutes || req.OffsetMinutes > MaxHistoryOffsetMinutes {
		return nil, public(fmt.Sprintf("tz_offset must be between %d and %d minutes", -MaxHistoryOffsetMinutes, MaxHistoryOffsetMinutes), ErrInvalid)
	}
	now := s.now()
	loc, zoneName := req.location()
	local := now.In(loc)
	// Midnights in loc: days are 23 or 25 hours long across daylight saving changes.
	starts := make([]time.Time, days+1)
	for i := range starts {
		starts[i] = time.Date(local.Year(), local.Month(), local.Day()-days+1+i, 0, 0, 0, 0, loc)
	}
	from := starts[0].UTC()
	_, offset := local.Zone()

	agg, err := s.cfg.Store.BackupHistory(ctx, store.BackupHistoryQuery{
		DayStarts: starts, RunsPerJob: HistoryRunsPerJob, MaxIssues: HistoryMaxVerificationIssues,
	})
	if err != nil {
		return nil, fmt.Errorf("backup history: %w", err)
	}
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	h := &History{
		Days: days, TimeZone: zoneName, OffsetMinutes: offset / 60, From: from, GeneratedAt: now.UTC(),
		Daily: make([]HistoryDay, 0, days), StoredBytesBefore: agg.BytesBefore,
		Jobs: map[string]HistoryJob{}, Upcoming: []UpcomingRun{}, VerificationIssues: []VerificationIssue{},
		VerificationIssuesTotal: agg.VerificationIssueTotal, ServerTimeZone: serverTimeZone(now),
	}
	byDay := make(map[int]store.BackupDay, len(agg.Days))
	for _, d := range agg.Days {
		byDay[d.Day] = d
	}
	stored := agg.BytesBefore
	for day := range days {
		d := byDay[day]
		stored += d.CompletedBytes
		h.Daily = append(h.Daily, HistoryDay{
			Date:      starts[day].Format(time.DateOnly),
			Completed: d.Completed, Failed: d.Failed, Cancelled: d.Cancelled,
			Bytes: d.CompletedBytes, StoredBytes: stored,
		})
	}
	for jobID, runs := range agg.JobRuns {
		hj := HistoryJob{Runs: make([]HistoryRun, 0, len(runs))}
		for _, r := range runs {
			hj.Runs = append(hj.Runs, HistoryRun{ID: r.ID, Status: r.Status, StartedAt: r.StartedAt, DurationSeconds: r.DurationSeconds})
		}
		h.Jobs[jobID] = hj
	}
	for jobID, at := range agg.LastSuccess {
		hj := h.Jobs[jobID]
		if hj.Runs == nil {
			hj.Runs = []HistoryRun{}
		}
		hj.LastSuccessAt = &at
		h.Jobs[jobID] = hj
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if runs := scheduler.NextRuns(j.CronExpression, now, 2); len(runs) == 2 {
			hj := h.Jobs[j.ID]
			if hj.Runs == nil {
				hj.Runs = []HistoryRun{}
			}
			hj.IntervalSeconds = runs[1].Sub(runs[0]).Seconds()
			h.Jobs[j.ID] = hj
		}
	}
	for _, j := range jobs {
		if j.MultiDatabase() {
			if err := s.multiJobHistory(ctx, j, h); err != nil {
				return nil, err
			}
		}
	}
	for _, v := range agg.VerificationIssues {
		h.VerificationIssues = append(h.VerificationIssues, VerificationIssue{
			ID: v.ID, JobID: v.JobID, Database: v.Database, StartedAt: v.StartedAt, Verification: v.Verification,
		})
	}
	h.Upcoming, h.UpcomingTruncated = upcomingRuns(jobs, now)
	return h, nil
}

// multiJobHistory replaces the per-backup history of multi-database job j with its
// runs (one entry per run, however many databases it backed up) and reports its
// stalest database as its last success.
func (s *Service) multiJobHistory(ctx context.Context, j *models.Job, h *History) error {
	list, err := s.cfg.Store.ListJobRuns(ctx, j.ID, HistoryRunsPerJob)
	if err != nil {
		return fmt.Errorf("runs of job %s: %w", j.ID, err)
	}
	hj := h.Jobs[j.ID]
	hj.Runs = make([]HistoryRun, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- { // oldest first
		run := list[i]
		ok, _, _, _ := run.Counts()
		hj.Runs = append(hj.Runs, HistoryRun{
			ID: run.ID, Status: runBackupStatus(run.Status), RunStatus: run.Status, StartedAt: run.StartedAt,
			DurationSeconds: run.DurationSeconds, Databases: len(run.Databases), Succeeded: ok,
			FailedDatabases: run.FailedDatabases(),
		})
	}
	var js JobStatus
	if err := s.multiJobStatus(ctx, j, &js); err != nil {
		return err
	}
	hj.LastSuccessAt, hj.StalestDatabase, hj.Databases = js.LastSuccessAt, js.StalestDatabase, js.Databases
	h.Jobs[j.ID] = hj
	return nil
}

// runBackupStatus is the backup status a job run's outcome is shown as.
func runBackupStatus(st models.JobRunStatus) models.BackupStatus {
	switch st {
	case models.JobRunOK:
		return models.StatusCompleted
	case models.JobRunCancelled:
		return models.StatusCancelled
	case models.JobRunRunning:
		return models.StatusInProgress
	case models.JobRunSkipped:
		return models.StatusSkipped
	default:
		return models.StatusFailed
	}
}

// upcomingRuns lists the activations of the enabled jobs in the next UpcomingWindow,
// in order, and reports whether MaxUpcomingPerJob or MaxUpcoming cut the list.
func upcomingRuns(jobs []*models.Job, now time.Time) ([]UpcomingRun, bool) {
	out := []UpcomingRun{}
	truncated := false
	end := now.Add(UpcomingWindow)
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		runs := scheduler.NextRuns(j.CronExpression, now, MaxUpcomingPerJob+1)
		for i, at := range runs {
			if at.After(end) {
				break
			}
			if i == MaxUpcomingPerJob {
				truncated = true
				break
			}
			out = append(out, UpcomingRun{JobID: j.ID, At: at})
		}
	}
	slices.SortStableFunc(out, func(a, b UpcomingRun) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.JobID, b.JobID)
	})
	if len(out) > MaxUpcoming {
		out, truncated = out[:MaxUpcoming], true
	}
	return out, truncated
}

// SchedulePreview is the reading of a cron expression.
type SchedulePreview struct {
	// Valid reports whether the scheduler accepts the expression.
	Valid bool `json:"valid"`
	// Error explains why it does not, when Valid is false.
	Error string `json:"error,omitempty"`
	// NextRuns are its next activations (UTC).
	NextRuns []time.Time `json:"next_runs"`
	// ServerTimeZone is the zone the expression is evaluated in.
	ServerTimeZone ServerTimeZone `json:"server_time_zone"`
	// DefaultRPOMinutes is the recovery point objective a job with this schedule has
	// when it sets none: two intervals plus an hour, at least six hours (0 when the
	// expression is not valid).
	DefaultRPOMinutes int `json:"default_rpo_minutes,omitempty"`
}

// PreviewSchedule reports whether expr is a valid schedule and its next n
// activations (1 to MaxSchedulePreviewRuns), as the scheduler computes them. An
// empty or overlong expression, or n out of range, returns an ErrInvalid error; an
// expression the scheduler rejects is reported in the result, not as an error.
func (s *Service) PreviewSchedule(expr string, n int) (*SchedulePreview, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || len(expr) > maxCronLength {
		return nil, public(fmt.Sprintf("cron must be 1 to %d characters", maxCronLength), ErrInvalid)
	}
	if n < 1 || n > MaxSchedulePreviewRuns {
		return nil, public(fmt.Sprintf("n must be between 1 and %d", MaxSchedulePreviewRuns), ErrInvalid)
	}
	now := s.now()
	p := &SchedulePreview{NextRuns: []time.Time{}, ServerTimeZone: serverTimeZone(now)}
	if err := scheduler.ValidateCron(expr); err != nil {
		p.Error = err.Error()
		return p, nil
	}
	p.Valid = true
	if runs := scheduler.NextRuns(expr, now, n); runs != nil {
		p.NextRuns = runs
	}
	p.DefaultRPOMinutes = int(scheduler.DefaultRPO(expr, now) / time.Minute)
	return p, nil
}

// serverTimeZone describes the local time zone at now.
func serverTimeZone(now time.Time) ServerTimeZone {
	local := now.Local()
	abbr, offset := local.Zone()
	name := local.Location().String()
	if name == "" || name == "Local" {
		name = abbr
	}
	return ServerTimeZone{Name: name, OffsetMinutes: offset / 60}
}
