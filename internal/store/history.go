package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// MaxHistoryRunsPerJob bounds BackupHistoryQuery.RunsPerJob.
const MaxHistoryRunsPerJob = 50

// MaxHistoryIssues bounds BackupHistoryQuery.MaxIssues.
const MaxHistoryIssues = 100

// MaxHistoryDays bounds the number of days of a BackupHistoryQuery.
const MaxHistoryDays = 366

// ErrInvalidHistory is returned by BackupHistory for day boundaries that are
// missing, too many or not increasing.
var ErrInvalidHistory = errors.New("store: invalid history query")

// BackupHistoryQuery selects what BackupHistory aggregates.
type BackupHistoryQuery struct {
	// DayStarts are the boundaries of the days, in increasing order: day i spans
	// [DayStarts[i], DayStarts[i+1]), so n days need n+1 boundaries (at most
	// MaxHistoryDays days). Callers compute them in the user's time zone, so days
	// follow its midnights also across daylight saving changes.
	DayStarts []time.Time
	// RunsPerJob is how many of each job's newest backups are returned (0 for none,
	// at most MaxHistoryRunsPerJob).
	RunsPerJob int
	// MaxIssues bounds the backups with a failed verification that are returned (at
	// most MaxHistoryIssues); VerificationIssueTotal counts all of them.
	MaxIssues int
	// Connections keeps the backups taken from these connections in the daily
	// figures, the stored size and the verification issues (the caller's access);
	// nil keeps every backup. JobRuns and LastSuccess are per job: callers keep the
	// jobs they may see.
	Connections auth.ConnectionSet
}

// connectionClause returns " AND col IN (...)" with its arguments for a limited
// set, " AND 0" for a limited set without members, and "" for nil.
func connectionClause(col string, set auth.ConnectionSet) (string, []any) {
	var c conditions
	c.addConnections(col, set)
	if len(c.clauses) == 0 {
		return "", nil
	}
	return " AND " + c.clauses[0], c.args
}

// BackupDay aggregates the backups that started on one day.
type BackupDay struct {
	// Day is the index of the day in BackupHistoryQuery.DayStarts.
	Day int
	// Completed, Failed and Cancelled count the backups of each final status.
	Completed, Failed, Cancelled int
	// CompletedBytes sums the size of the completed ones.
	CompletedBytes int64
}

// JobRun is one backup of a job, as shown in its recent outcomes.
type JobRun struct {
	// ID is the backup ID.
	ID string
	// Status is its state.
	Status models.BackupStatus
	// StartedAt is when it started.
	StartedAt time.Time
	// DurationSeconds is how long it ran (0 while it runs or when unknown).
	DurationSeconds float64
}

// VerificationIssue is a completed backup whose last verification failed.
type VerificationIssue struct {
	// ID is the backup ID.
	ID string
	// JobID is its job, or "".
	JobID string
	// Database is the backed-up database.
	Database string
	// StartedAt is when the backup started.
	StartedAt time.Time
	// Verification is models.VerificationMismatch or models.VerificationError.
	Verification models.VerificationStatus
}

// BackupHistory holds SQL aggregates of the backups over time.
type BackupHistory struct {
	// Days lists the days that have backups, in day order.
	Days []BackupDay
	// BytesBefore sums the size of completed backups that started before the first
	// day.
	BytesBefore int64
	// JobRuns maps the ID of each job (in the jobs table) with backups to its
	// newest backups, oldest first.
	JobRuns map[string][]JobRun
	// LastSuccess maps the ID of each job with a completed backup to the start time
	// of its newest one.
	LastSuccess map[string]time.Time
	// VerificationIssues lists the newest completed backups of the window (started
	// on or after the first day) whose verification failed (mismatch or error),
	// newest first.
	VerificationIssues []VerificationIssue
	// VerificationIssueTotal counts all of them in the window.
	VerificationIssueTotal int
}

// The history's queries. Each one is answered from an index on backups
// (started_at, (status, started_at) or (job_id, started_at)) and never scans the
// table; JSON is read only from the rows the index selected (TestHistoryQueryPlans).
const (
	// historyBytesBeforeSQL sums the completed backups that started before ?.
	historyBytesBeforeSQL = `SELECT coalesce(sum(size_bytes), 0) FROM backups WHERE status = ? AND started_at < ?`
	// historyRunsSQL lists each job's ? newest backups: one index search per job,
	// the duration read only from those rows.
	historyRunsSQL = `SELECT b.job_id, b.id, b.status, b.started_at,
			CAST(coalesce(json_extract(b.data, '$.duration_seconds'), 0) AS REAL)
		FROM jobs j JOIN backups b ON b.id IN (
			SELECT id FROM backups WHERE job_id = j.id ORDER BY started_at DESC, id DESC LIMIT ?)
		ORDER BY b.job_id, b.started_at, b.id`
	// historyLastSuccessSQL finds each job's newest completed backup by walking its
	// backups newest first (job index) until the first completed one; "+status"
	// keeps the planner from walking every completed backup through the status index.
	historyLastSuccessSQL = `SELECT j.id, (SELECT started_at FROM backups
			WHERE job_id = j.id AND +status = ? ORDER BY started_at DESC, id DESC LIMIT 1)
		FROM jobs j`
	// historyVerificationSQL lists the completed backups that started at or after ?
	// and failed verification, with their count; the verification is extracted once
	// per completed backup of the window, which the (status, started_at) index selects
	// (the planner would otherwise prefer a full scan when the window holds many rows).
	historyVerificationSQL = `WITH w AS MATERIALIZED (
			SELECT id, job_id, database_name, started_at, json_extract(data, '$.verification') AS verification
			FROM backups INDEXED BY backups_by_status_started WHERE status = ? AND started_at >= ?)
		SELECT id, job_id, database_name, started_at, verification, count(*) OVER ()
		FROM w WHERE verification IN (?, ?) ORDER BY started_at DESC, id DESC LIMIT ?`
)

// historyDaysSQL aggregates the backups per day of n days: day boundaries as a
// VALUES table, one started_at index range per day.
func historyDaysSQL(n int) string {
	rows := strings.TrimSuffix(strings.Repeat("(?, ?, ?),", n), ",")
	return `WITH d(day, lo, hi) AS (VALUES ` + rows + `)
		SELECT d.day, b.status, count(*), coalesce(sum(b.size_bytes), 0)
		FROM d JOIN backups b ON b.started_at >= d.lo AND b.started_at < d.hi
		GROUP BY d.day, b.status ORDER BY d.day`
}

// BackupHistory returns the per-day outcomes and sizes of the backups in the days
// of q, the size stored before them, every job's newest runs and last success, and
// the window's completed backups whose verification failed. Everything is computed
// with SQL over indexes; JSON is read only from the selected rows.
func (s *SQLiteStore) BackupHistory(ctx context.Context, q BackupHistoryQuery) (*BackupHistory, error) {
	n := len(q.DayStarts) - 1
	if n < 1 || n > MaxHistoryDays {
		return nil, fmt.Errorf("%w: %d day boundaries", ErrInvalidHistory, len(q.DayStarts))
	}
	for i := 1; i <= n; i++ {
		if !q.DayStarts[i].After(q.DayStarts[i-1]) {
			return nil, fmt.Errorf("%w: day boundaries must increase", ErrInvalidHistory)
		}
	}
	h := &BackupHistory{JobRuns: map[string][]JobRun{}, LastSuccess: map[string]time.Time{}}
	if err := s.historyDays(ctx, h, q.DayStarts, q.Connections); err != nil {
		return nil, err
	}
	from := timeKey(q.DayStarts[0])
	clause, connArgs := connectionClause("connection_id", q.Connections)
	if err := s.db.QueryRowContext(ctx, historyBytesBeforeSQL+clause, append([]any{string(models.StatusCompleted), from}, connArgs...)...).Scan(&h.BytesBefore); err != nil {
		return nil, fmt.Errorf("store: stored bytes before the history: %w", err)
	}
	if perJob := min(max(q.RunsPerJob, 0), MaxHistoryRunsPerJob); perJob > 0 {
		if err := s.historyJobRuns(ctx, h, perJob); err != nil {
			return nil, err
		}
	}
	if err := s.historyLastSuccess(ctx, h); err != nil {
		return nil, err
	}
	if err := s.historyVerification(ctx, h, from, min(max(q.MaxIssues, 0), MaxHistoryIssues), q.Connections); err != nil {
		return nil, err
	}
	return h, nil
}

func (s *SQLiteStore) historyDays(ctx context.Context, h *BackupHistory, starts []time.Time, set auth.ConnectionSet) error {
	n := len(starts) - 1
	args := make([]any, 0, 3*n)
	for i := range n {
		args = append(args, i, timeKey(starts[i]), timeKey(starts[i+1]))
	}
	query := historyDaysSQL(n)
	if clause, connArgs := connectionClause("b.connection_id", set); clause != "" {
		query = strings.Replace(query, "AND b.started_at < d.hi", "AND b.started_at < d.hi"+clause, 1)
		args = append(args, connArgs...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: backup history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			day    int
			status string
			count  int
			bytes  int64
		)
		if err := rows.Scan(&day, &status, &count, &bytes); err != nil {
			return fmt.Errorf("store: scan backup history: %w", err)
		}
		if len(h.Days) == 0 || h.Days[len(h.Days)-1].Day != day {
			h.Days = append(h.Days, BackupDay{Day: day})
		}
		d := &h.Days[len(h.Days)-1]
		switch models.BackupStatus(status) {
		case models.StatusCompleted:
			d.Completed += count
			d.CompletedBytes += bytes
		case models.StatusFailed:
			d.Failed += count
		case models.StatusCancelled:
			d.Cancelled += count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read backup history: %w", err)
	}
	return nil
}

func (s *SQLiteStore) historyJobRuns(ctx context.Context, h *BackupHistory, perJob int) error {
	rows, err := s.db.QueryContext(ctx, historyRunsSQL, perJob)
	if err != nil {
		return fmt.Errorf("store: recent job runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			jobID, id, status string
			started           int64
			duration          float64
		)
		if err := rows.Scan(&jobID, &id, &status, &started, &duration); err != nil {
			return fmt.Errorf("store: scan recent job runs: %w", err)
		}
		h.JobRuns[jobID] = append(h.JobRuns[jobID], JobRun{
			ID: id, Status: models.BackupStatus(status), StartedAt: time.Unix(0, started).UTC(), DurationSeconds: max(duration, 0),
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read recent job runs: %w", err)
	}
	return nil
}

func (s *SQLiteStore) historyLastSuccess(ctx context.Context, h *BackupHistory) error {
	rows, err := s.db.QueryContext(ctx, historyLastSuccessSQL, string(models.StatusCompleted))
	if err != nil {
		return fmt.Errorf("store: last successful job runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			jobID   string
			started sql.NullInt64
		)
		if err := rows.Scan(&jobID, &started); err != nil {
			return fmt.Errorf("store: scan last successful job runs: %w", err)
		}
		if started.Valid {
			h.LastSuccess[jobID] = time.Unix(0, started.Int64).UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read last successful job runs: %w", err)
	}
	return nil
}

func (s *SQLiteStore) historyVerification(ctx context.Context, h *BackupHistory, from int64, limit int, set auth.ConnectionSet) error {
	if limit == 0 {
		// Only the count is wanted: one row is enough to carry it.
		limit = 1
		defer func() { h.VerificationIssues = nil }()
	}
	query, args := historyVerificationSQL, []any{string(models.StatusCompleted), from}
	if clause, connArgs := connectionClause("connection_id", set); clause != "" {
		query = strings.Replace(query, "WHERE status = ? AND started_at >= ?)", "WHERE status = ? AND started_at >= ?"+clause+")", 1)
		args = append(args, connArgs...)
	}
	args = append(args, string(models.VerificationMismatch), string(models.VerificationError), limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: failed verifications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			v       VerificationIssue
			started int64
			status  string
		)
		if err := rows.Scan(&v.ID, &v.JobID, &v.Database, &started, &status, &h.VerificationIssueTotal); err != nil {
			return fmt.Errorf("store: scan failed verifications: %w", err)
		}
		v.StartedAt, v.Verification = time.Unix(0, started).UTC(), models.VerificationStatus(status)
		h.VerificationIssues = append(h.VerificationIssues, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read failed verifications: %w", err)
	}
	return nil
}
