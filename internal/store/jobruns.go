package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

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
		job.KnownDatabases = slices.Clone(known)
		data, err := encode(job)
		if err != nil {
			return err
		}
		return execOne(ctx, tx, ErrNotFound, "UPDATE jobs SET data = ? WHERE id = ?", data, id)
	})
}
