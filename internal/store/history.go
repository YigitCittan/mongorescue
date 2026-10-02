package store

import (
	"context"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// MaxHistoryRunsPerJob bounds BackupHistoryQuery.RunsPerJob.
const MaxHistoryRunsPerJob = 50

// MaxHistoryIssues bounds BackupHistoryQuery.MaxIssues.
const MaxHistoryIssues = 100

// BackupHistoryQuery selects what BackupHistory aggregates.
type BackupHistoryQuery struct {
	// From is the start of the first day; backups that started earlier only count
	// towards BytesBefore.
	From time.Time
	// OffsetSeconds shifts start times before they are cut into days, so days follow
	// the caller's time zone (east of UTC is positive).
	OffsetSeconds int64
	// RunsPerJob is how many of each job's newest backups are returned (0 for none,
	// at most MaxHistoryRunsPerJob).
	RunsPerJob int
	// MaxIssues bounds the backups with a failed verification that are returned (at
	// most MaxHistoryIssues); VerificationIssueTotal counts all of them.
	MaxIssues int
}

// BackupDay aggregates the backups that started on one day.
type BackupDay struct {
	// Day is the day number: whole days since 1970-01-01 in the query's time zone.
	Day int64
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
	// Days lists the days from the query's From that have backups, oldest first.
	Days []BackupDay
	// BytesBefore sums the size of completed backups that started before From.
	BytesBefore int64
	// JobRuns maps each job ID to its newest backups, oldest first.
	JobRuns map[string][]JobRun
	// LastSuccess maps each job ID with a completed backup to the start time of its
	// newest one.
	LastSuccess map[string]time.Time
	// VerificationIssues lists the newest completed backups whose verification
	// failed (mismatch or error), newest first.
	VerificationIssues []VerificationIssue
	// VerificationIssueTotal counts all of them.
	VerificationIssueTotal int
}

// BackupHistory returns the per-day outcomes and sizes of the backups that started
// at or after q.From, the size stored before it, every job's newest runs and last
// success, and the completed backups whose verification failed. Everything is
// computed with SQL aggregates; no record is decoded.
func (s *SQLiteStore) BackupHistory(ctx context.Context, q BackupHistoryQuery) (*BackupHistory, error) {
	h := &BackupHistory{JobRuns: map[string][]JobRun{}, LastSuccess: map[string]time.Time{}}
	from := timeKey(q.From)
	if err := s.historyDays(ctx, h, from, q.OffsetSeconds); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(size_bytes), 0) FROM backups WHERE status = ? AND started_at < ?`,
		string(models.StatusCompleted), from).Scan(&h.BytesBefore); err != nil {
		return nil, fmt.Errorf("store: stored bytes before the history: %w", err)
	}
	if n := min(max(q.RunsPerJob, 0), MaxHistoryRunsPerJob); n > 0 {
		if err := s.historyJobRuns(ctx, h, n); err != nil {
			return nil, err
		}
	}
	if err := s.historyLastSuccess(ctx, h); err != nil {
		return nil, err
	}
	if err := s.historyVerification(ctx, h, min(max(q.MaxIssues, 0), MaxHistoryIssues)); err != nil {
		return nil, err
	}
	return h, nil
}

func (s *SQLiteStore) historyDays(ctx context.Context, h *BackupHistory, from, offset int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT (started_at / 1000000000 + ?) / 86400 AS day, status, count(*), coalesce(sum(size_bytes), 0)
		FROM backups WHERE started_at >= ? GROUP BY day, status ORDER BY day`, offset, from)
	if err != nil {
		return fmt.Errorf("store: backup history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			day    int64
			status string
			n      int
			bytes  int64
		)
		if err := rows.Scan(&day, &status, &n, &bytes); err != nil {
			return fmt.Errorf("store: scan backup history: %w", err)
		}
		if len(h.Days) == 0 || h.Days[len(h.Days)-1].Day != day {
			h.Days = append(h.Days, BackupDay{Day: day})
		}
		d := &h.Days[len(h.Days)-1]
		switch models.BackupStatus(status) {
		case models.StatusCompleted:
			d.Completed += n
			d.CompletedBytes += bytes
		case models.StatusFailed:
			d.Failed += n
		case models.StatusCancelled:
			d.Cancelled += n
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read backup history: %w", err)
	}
	return nil
}

func (s *SQLiteStore) historyJobRuns(ctx context.Context, h *BackupHistory, perJob int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, id, status, started_at, duration FROM (
			SELECT job_id, id, status, started_at,
				CAST(coalesce(json_extract(data, '$.duration_seconds'), 0) AS REAL) AS duration,
				row_number() OVER (PARTITION BY job_id ORDER BY started_at DESC, id DESC) AS rn
			FROM backups WHERE job_id != ''
		) WHERE rn <= ? ORDER BY job_id, started_at, id`, perJob)
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
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, max(started_at) FROM backups
		WHERE job_id != '' AND status = ? GROUP BY job_id`, string(models.StatusCompleted))
	if err != nil {
		return fmt.Errorf("store: last successful job runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			jobID   string
			started int64
		)
		if err := rows.Scan(&jobID, &started); err != nil {
			return fmt.Errorf("store: scan last successful job runs: %w", err)
		}
		h.LastSuccess[jobID] = time.Unix(0, started).UTC()
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read last successful job runs: %w", err)
	}
	return nil
}

func (s *SQLiteStore) historyVerification(ctx context.Context, h *BackupHistory, limit int) error {
	const where = `FROM backups WHERE status = ? AND json_extract(data, '$.verification') IN (?, ?)`
	args := []any{string(models.StatusCompleted), string(models.VerificationMismatch), string(models.VerificationError)}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+where, args...).Scan(&h.VerificationIssueTotal); err != nil {
		return fmt.Errorf("store: count failed verifications: %w", err)
	}
	if limit == 0 || h.VerificationIssueTotal == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, database_name, started_at, json_extract(data, '$.verification') `+where+
		` ORDER BY started_at DESC, id DESC LIMIT ?`, append(args, limit)...)
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
		if err := rows.Scan(&v.ID, &v.JobID, &v.Database, &started, &status); err != nil {
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
