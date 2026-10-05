package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// tableJobRuns is the table of job runs.
const tableJobRuns = "job_runs"

// MaxJobRunList is the most job runs ListJobRuns returns.
const MaxJobRunList = 200

// SaveJobRun creates or replaces a job run.
func (s *SQLiteStore) SaveJobRun(ctx context.Context, run *models.JobRun) error {
	if run == nil || run.ID == "" || run.JobID == "" {
		return fmt.Errorf("%w: job run with ID and job ID is required", ErrInvalidRecord)
	}
	data, err := encode(run)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO job_runs (id, job_id, started_at, status, data) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET job_id = excluded.job_id, started_at = excluded.started_at,
			status = excluded.status, data = excluded.data`,
		run.ID, run.JobID, timeKey(run.StartedAt), string(run.Status), data); err != nil {
		return fmt.Errorf("store: save job run %s: %w", run.ID, err)
	}
	return nil
}

// GetJobRun returns a job run or ErrNotFound.
func (s *SQLiteStore) GetJobRun(ctx context.Context, id string) (*models.JobRun, error) {
	return getRecord[models.JobRun](ctx, s.db, ErrNotFound, "SELECT data FROM job_runs WHERE id = ?", id)
}

// ListJobRuns returns up to limit (1 to MaxJobRunList; other values mean
// MaxJobRunList) runs of job jobID, newest first.
func (s *SQLiteStore) ListJobRuns(ctx context.Context, jobID string, limit int) ([]*models.JobRun, error) {
	if limit <= 0 || limit > MaxJobRunList {
		limit = MaxJobRunList
	}
	return listRecords[models.JobRun](ctx, s, tableJobRuns, nil,
		"SELECT id, data FROM job_runs WHERE job_id = ? ORDER BY started_at DESC, id DESC LIMIT ?", jobID, limit)
}

// latestJobRunsChunk bounds the job IDs of one LatestJobRuns query (SQL variables).
const latestJobRunsChunk = 500

// LatestJobRuns maps each of jobIDs that has runs to its newest run (by start time,
// then ID, like ListJobRuns(ctx, id, 1)), in one query per 500 IDs that reads the
// job_runs_by_job index. A job whose newest run cannot be read is left out (and the
// row reported), as ListJobRuns(ctx, id, 1) returns no run for it. Skipped runs
// (models.JobRunSkipped, such as a scheduled run outside the job's backup window)
// never started and are passed over: the newest run is the newest that ran.
func (s *SQLiteStore) LatestJobRuns(ctx context.Context, jobIDs []string) (map[string]*models.JobRun, error) {
	out := make(map[string]*models.JobRun, len(jobIDs))
	for start := 0; start < len(jobIDs); start += latestJobRunsChunk {
		chunk := jobIDs[start:min(start+latestJobRunsChunk, len(jobIDs))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		// One constant "?" per ID; the IDs themselves are arguments.
		query := `SELECT id, data FROM (
				SELECT id, data, row_number() OVER (PARTITION BY job_id ORDER BY started_at DESC, id DESC) AS rn
				FROM job_runs WHERE status != '` + string(models.JobRunSkipped) + `' AND job_id IN (?` + strings.Repeat(", ?", len(chunk)-1) + `)
			) WHERE rn = 1`
		list, err := listRecords[models.JobRun](ctx, s, tableJobRuns, nil, query, args...)
		if err != nil {
			return nil, fmt.Errorf("store: latest job runs: %w", err)
		}
		for _, run := range list {
			out[run.JobID] = run
		}
	}
	return out, nil
}

// ListRunningJobRuns returns every job run still recorded as running, oldest first.
func (s *SQLiteStore) ListRunningJobRuns(ctx context.Context) ([]*models.JobRun, error) {
	return listRecords[models.JobRun](ctx, s, tableJobRuns, nil,
		"SELECT id, data FROM job_runs WHERE status = ? ORDER BY started_at, id", string(models.JobRunRunning))
}

// UpdateJobKnownDatabases reads job id and, in the same transaction, stores the
// known databases update returns for it as its KnownDatabases, without touching its
// settings or UpdatedAt (a run finishing never reverts an edit saved meanwhile).
// update sees the job as it is stored now and returns write false to leave it as it
// is. It returns ErrNotFound when the job was deleted.
func (s *SQLiteStore) UpdateJobKnownDatabases(ctx context.Context, id string, update func(job *models.Job) (known []string, write bool)) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		job, err := getRecord[models.Job](ctx, tx, ErrNotFound, "SELECT data FROM jobs WHERE id = ?", id)
		if err != nil {
			return err
		}
		known, write := update(job.Clone())
		if !write {
			return nil
		}
		if known == nil {
			known = []string{}
		}
		// Databases that join a job which already had known databases are recorded
		// with the time they joined: their RPO age counts from then.
		if job.KnownDatabases != nil {
			joined := timeKey(time.Now().UTC())
			for _, db := range known {
				if slices.Contains(job.KnownDatabases, db) {
					continue
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO job_database_joins (job_id, database_name, joined_at) VALUES (?, ?, ?)
					ON CONFLICT (job_id, database_name) DO NOTHING`, id, db, joined); err != nil {
					return fmt.Errorf("store: record the database joining job %s: %w", id, err)
				}
			}
		}
		job.KnownDatabases = slices.Clone(known)
		data, err := encode(job)
		if err != nil {
			return err
		}
		return execOne(ctx, tx, ErrNotFound, "UPDATE jobs SET data = ? WHERE id = ?", data, id)
	})
}
