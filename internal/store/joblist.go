package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// MaxJobWindow is the largest JobQuery.Limit.
const MaxJobWindow = 1000

// jobBatchIDs bounds the job IDs of one batched query (SQL variables).
const jobBatchIDs = 500

// JobQuery selects a window of jobs in name and ID order, for keyset paging. Zero
// fields match every job; all set fields must match.
type JobQuery struct {
	// AfterName and AfterID start the window after the job with this name and ID in
	// name and ID order (the key of the last job of the previous window); an empty
	// AfterID starts at the first job.
	AfterName, AfterID string
	// Limit is the most jobs the window reads (1 to MaxJobWindow); 0 reads every
	// match after the cursor.
	Limit int
	// ConnectionID keeps the jobs of this connection.
	ConnectionID string
	// Enabled keeps the enabled (true) or paused (false) jobs; nil keeps both.
	Enabled *bool
	// Connections keeps the jobs of these connections (the caller's access, see
	// auth.ConnectionFilter); nil keeps every job.
	Connections auth.ConnectionSet
}

// JobWindow is one window of QueryJobs.
type JobWindow struct {
	// Jobs are the readable jobs of the window, heartbeat URLs decrypted. A row
	// that cannot be read is skipped and reported, as ListJobs does.
	Jobs []*models.Job
	// LastName and LastID are the key of the last row read, also when it was
	// skipped: the cursor of the next window.
	LastName, LastID string
	// More reports that matching rows follow the window.
	More bool
}

// jobConditions returns the filters of q, without its cursor.
func jobConditions(q JobQuery) *conditions {
	c := &conditions{}
	if q.ConnectionID != "" {
		c.add("connection_id = ?", q.ConnectionID)
	}
	if q.Enabled != nil {
		c.add("enabled = ?", boolInt(*q.Enabled))
	}
	c.addConnections("connection_id", q.Connections)
	return c
}

// jobWindowSQL returns the query of QueryJobs and its arguments.
func jobWindowSQL(q JobQuery) (string, []any) {
	c := jobConditions(q)
	if q.AfterID != "" {
		c.add("(name, id) > (?, ?)", q.AfterName, q.AfterID)
	}
	query := "SELECT id, name, data FROM jobs" + c.where() + " ORDER BY name, id"
	args := c.args
	if q.Limit > 0 {
		// One row more than the window tells whether another one follows.
		query += " LIMIT ?"
		args = append(args, q.Limit+1)
	}
	return query, args
}

// jobCountSQL returns the query of CountJobs and its arguments.
func jobCountSQL(q JobQuery) (string, []any) {
	c := jobConditions(q)
	return "SELECT count(*) FROM jobs" + c.where(), c.args
}

// QueryJobs reads the jobs matching q after its cursor, in name and ID order (the
// jobs_by_name index, or a connection index of the same order), at most q.Limit of
// them. It returns an ErrInvalidFilter error for a limit out of range.
func (s *SQLiteStore) QueryJobs(ctx context.Context, q JobQuery) (*JobWindow, error) {
	if q.Limit < 0 || q.Limit > MaxJobWindow {
		return nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidFilter, MaxJobWindow)
	}
	query, args := jobWindowSQL(q)
	defer s.lockKey()()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	w := &JobWindow{Jobs: []*models.Job{}}
	read := 0
	for rows.Next() {
		if q.Limit > 0 && read == q.Limit {
			w.More = true
			break
		}
		var id, name, data string
		if err := rows.Scan(&id, &name, &data); err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		read++
		w.LastName, w.LastID = name, id
		job, err := decode[models.Job](data)
		if err == nil {
			if err = s.openJob(job); errors.Is(err, ErrNoSecretBox) {
				return nil, err
			}
		}
		if err != nil {
			s.reportCorrupt(tableJobs, id, err)
			continue
		}
		s.clearCorrupt(tableJobs, id)
		w.Jobs = append(w.Jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate jobs: %w", err)
	}
	return w, nil
}

// CountJobs counts the jobs matching the filters of q (its cursor and limit are
// ignored), from the jobs indexes without reading a job.
func (s *SQLiteStore) CountJobs(ctx context.Context, q JobQuery) (int, error) {
	query, args := jobCountSQL(q)
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count jobs: %w", err)
	}
	return n, nil
}

// idValues returns a VALUES table "j(id)" of n placeholders, for a batch of IDs.
func idValues(n int) string {
	return "j(id) AS (VALUES (?)" + strings.Repeat(", (?)", n-1) + ")"
}

// idArgs returns ids as query arguments.
func idArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// RecentExecutedJobRuns maps each of jobIDs that has runs to its newest perJob runs
// that ran (ListExecutedJobRuns for many jobs; perJob out of 1 to MaxJobRunList
// means MaxJobRunList), newest first. It reads one bounded index range per job, in
// one query per 500 IDs; unreadable runs are skipped and reported.
func (s *SQLiteStore) RecentExecutedJobRuns(ctx context.Context, jobIDs []string, perJob int) (map[string][]*models.JobRun, error) {
	if perJob <= 0 || perJob > MaxJobRunList {
		perJob = MaxJobRunList
	}
	out := make(map[string][]*models.JobRun, len(jobIDs))
	for start := 0; start < len(jobIDs); start += jobBatchIDs {
		chunk := jobIDs[start:min(start+jobBatchIDs, len(jobIDs))]
		// One constant "?" per ID; the IDs themselves are arguments. Per job, the
		// job_runs_by_job index yields its newest runs, perJob of them at most.
		query := `WITH ` + idValues(len(chunk)) + `
			SELECT r.id, r.data FROM j JOIN job_runs r ON r.id IN (
				SELECT id FROM job_runs WHERE job_id = j.id AND status != ? ORDER BY started_at DESC, id DESC LIMIT ?)
			ORDER BY r.job_id, r.started_at DESC, r.id DESC`
		args := append(idArgs(chunk), string(models.JobRunSkipped), perJob)
		list, err := listRecords[models.JobRun](ctx, s, tableJobRuns, nil, query, args...)
		if err != nil {
			return nil, fmt.Errorf("store: recent job runs: %w", err)
		}
		for _, run := range list {
			out[run.JobID] = append(out[run.JobID], run)
		}
	}
	return out, nil
}

// latestJobsDatabaseBackupsSQL selects the newest backup per job and database of n
// job IDs, then the status twice for each side (an empty status matches every
// status, since no record has an empty one).
func latestJobsDatabaseBackupsSQL(n int) string {
	return `WITH ` + idValues(n) + `
		SELECT b.id, b.data FROM backups b JOIN (
			SELECT i.job_id, i.database_name, max(i.started_at) AS latest FROM j JOIN backups i ON i.job_id = j.id
			WHERE (? = '' OR i.status = ?) GROUP BY i.job_id, i.database_name
		) l ON b.job_id = l.job_id AND b.database_name = l.database_name AND b.started_at = l.latest
		WHERE (? = '' OR b.status = ?) ORDER BY b.started_at DESC, b.id DESC`
}

// LatestJobsDatabaseBackups maps each of jobIDs to the newest backup in status (any
// status when empty) of each of its databases: LatestJobDatabaseBackups for many
// jobs, in one query per 500 IDs over the (job_id, database_name, started_at) index.
func (s *SQLiteStore) LatestJobsDatabaseBackups(ctx context.Context, jobIDs []string, status models.BackupStatus) (map[string]map[string]*models.BackupRecord, error) {
	out := make(map[string]map[string]*models.BackupRecord, len(jobIDs))
	st := string(status)
	for start := 0; start < len(jobIDs); start += jobBatchIDs {
		chunk := jobIDs[start:min(start+jobBatchIDs, len(jobIDs))]
		args := append(idArgs(chunk), st, st, st, st)
		list, err := listRecords[models.BackupRecord](ctx, s, tableBackups, nil, latestJobsDatabaseBackupsSQL(len(chunk)), args...)
		if err != nil {
			return nil, fmt.Errorf("store: latest database backups of jobs: %w", err)
		}
		for _, b := range list {
			if out[b.JobID] == nil {
				out[b.JobID] = map[string]*models.BackupRecord{}
			}
			if _, seen := out[b.JobID][b.Database]; !seen {
				out[b.JobID][b.Database] = b
			}
		}
	}
	return out, nil
}
