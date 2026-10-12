package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// MaxJobWindow is the largest JobQuery.Limit.
const MaxJobWindow = 1000

// jobBatchIDs bounds the job IDs of one batched query (SQL variables).
const jobBatchIDs = 500

// JobSort is an order of QueryJobs; ties are broken by ID.
type JobSort string

// The orders of QueryJobs.
const (
	// JobSortName orders by name (the default).
	JobSortName JobSort = "name"
	// JobSortCreatedAt orders by creation time.
	JobSortCreatedAt JobSort = "created_at"
	// JobSortNextRun orders by the next scheduled run; paused jobs and jobs without
	// one come last in both directions.
	JobSortNextRun JobSort = "next_run"
)

// JobKey is the position of a job in a JobSort order: its sort value and its ID.
// It is the cursor of keyset paging.
type JobKey struct {
	// Null marks a job without a value (JobSortNextRun: paused, or never
	// scheduled); such jobs come after the others in both directions.
	Null bool `json:"n,omitempty"`
	// Str, Int and Real hold the value: the name, the creation time in Unix
	// nanoseconds, or the next run as a Julian day number (SQLite's julianday).
	// The fields an order does not use stay zero.
	Str  string  `json:"k,omitempty"`
	Int  int64   `json:"i,omitempty"`
	Real float64 `json:"r,omitempty"`
	// ID breaks ties.
	ID string `json:"id"`
}

// CompareJobKeys orders two keys as QueryJobs does: keys without a value last,
// then by value and ID, ascending or (desc) descending.
func CompareJobKeys(a, b JobKey, desc bool) int {
	if a.Null != b.Null {
		if a.Null {
			return 1
		}
		return -1
	}
	c := cmp.Or(strings.Compare(a.Str, b.Str), cmp.Compare(a.Int, b.Int), cmp.Compare(a.Real, b.Real), strings.Compare(a.ID, b.ID))
	if desc {
		return -c
	}
	return c
}

// JobKeyOf returns the key of job j in order sort, as QueryJobs computes it.
func JobKeyOf(j *models.Job, sort JobSort) JobKey {
	k := JobKey{ID: j.ID}
	switch sort {
	case JobSortCreatedAt:
		k.Int = timeKey(j.CreatedAt)
	case JobSortNextRun:
		if !j.Enabled || j.NextRun == nil {
			k.Null = true
		} else {
			k.Real = julianDay(*j.NextRun)
		}
	default:
		k.Str = j.Name
	}
	return k
}

// julianDay is SQLite's julianday() of t: the milliseconds since the Julian epoch
// (rounded, as SQLite reads fractional seconds), in days.
func julianDay(t time.Time) float64 {
	ms := (t.UnixNano() + 500_000) / 1_000_000
	return float64(ms+210_866_760_000_000) / 86_400_000.0
}

// nextRunSQL is the JobSortNextRun value of a row (NULL without a next run);
// nextRunSetSQL holds for the rows that have one (indexed by jobs_by_next_run and
// jobs_by_connection_next_run).
const (
	nextRunSQL    = "julianday(json_extract(data, '$.next_run'))"
	nextRunSetSQL = "enabled = 1 AND " + nextRunSQL + " IS NOT NULL"
)

// JobQuery selects a window of jobs in a sort order, for keyset paging. Zero fields
// match every job; all set fields must match.
type JobQuery struct {
	// Sort is the order (JobSortName when empty); Desc reverses it, except that
	// jobs without a value stay last.
	Sort JobSort
	Desc bool
	// After starts the window after the job with this key in the order (the Last
	// of the previous window); nil starts at the first job.
	After *JobKey
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
	// Last is the key of the last row read, also when it was skipped: the cursor
	// of the next window.
	Last JobKey
	// More reports that matching rows follow the window.
	More bool
}

// jobConditions returns the filters of q, without its cursor.
func jobConditions(q JobQuery) *conditions {
	c := &conditions{}
	switch {
	case q.ConnectionID == "":
		c.addConnections("connection_id", q.Connections)
	case q.Connections.Allows(q.ConnectionID):
		// The connection is one of the caller's: its own condition is enough (an IN
		// list on the same column would cost the index its order).
		c.add("connection_id = ?", q.ConnectionID)
	default:
		c.add("0")
	}
	if q.Enabled != nil {
		c.add("enabled = ?", boolInt(*q.Enabled))
	}
	return c
}

// jobSegment is one query of a window: the rows with a sort value, or (null) those
// without one, in order.
type jobSegment struct {
	query string
	args  []any
	null  bool
}

// validJobSort reports whether sort is an order of QueryJobs ("" is JobSortName).
func validJobSort(sort JobSort) bool {
	switch sort {
	case "", JobSortName, JobSortCreatedAt, JobSortNextRun:
		return true
	}
	return false
}

// jobWindowSQL returns the queries of a QueryJobs window, run in turn: one for the
// orders whose rows all have a value, and for JobSortNextRun the rows with a next
// run, then those without (never before the cursor's segment). Every query reads
// "id, value, data" in the order of an index; the cursor and the order are
// constant SQL, the values arguments.
func jobWindowSQL(q JobQuery) []jobSegment {
	dir, op := "", ">"
	if q.Desc {
		dir, op = " DESC", "<"
	}
	var segs []jobSegment
	value := func(expr string, null bool) {
		c := jobConditions(q)
		switch {
		case null:
			c.add("NOT (" + nextRunSetSQL + ")")
		case q.Sort == JobSortNextRun:
			c.add(nextRunSetSQL)
		}
		if a := q.After; a != nil && a.Null == null {
			if null {
				c.add("id "+op+" ?", a.ID)
			} else {
				c.add("("+expr+", id) "+op+" (?, ?)", afterValue(q.Sort, *a), a.ID)
			}
		}
		order := expr + dir + ", id" + dir
		col := expr
		if null {
			order, col = "id"+dir, "NULL"
		}
		from := "jobs"
		if q.Sort == JobSortNextRun && !null {
			// The planner prefers the enabled index and a sort of every enabled job;
			// the partial index on the next run reads only the window.
			from = "jobs INDEXED BY jobs_by_next_run"
			if q.ConnectionID != "" {
				from = "jobs INDEXED BY jobs_by_connection_next_run"
			}
		}
		segs = append(segs, jobSegment{query: "SELECT id, " + col + ", data FROM " + from + c.where() + " ORDER BY " + order, args: c.args, null: null})
	}
	switch q.Sort {
	case JobSortCreatedAt:
		value("created_at", false)
	case JobSortNextRun:
		if q.After == nil || !q.After.Null {
			value(nextRunSQL, false)
		}
		value(nextRunSQL, true)
	default:
		value("name", false)
	}
	return segs
}

// afterValue returns the sort value of cursor a as a query argument.
func afterValue(sort JobSort, a JobKey) any {
	switch sort {
	case JobSortCreatedAt:
		return a.Int
	case JobSortNextRun:
		return a.Real
	}
	return a.Str
}

// jobCountSQL returns the query of CountJobs and its arguments.
func jobCountSQL(q JobQuery) (string, []any) {
	c := jobConditions(q)
	return "SELECT count(*) FROM jobs" + c.where(), c.args
}

// QueryJobs reads the jobs matching q after its cursor, in its order (from the jobs
// indexes of that order), at most q.Limit of them. It returns an ErrInvalidFilter
// error for a limit or an order out of range.
func (s *SQLiteStore) QueryJobs(ctx context.Context, q JobQuery) (*JobWindow, error) {
	if q.Limit < 0 || q.Limit > MaxJobWindow {
		return nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidFilter, MaxJobWindow)
	}
	if !validJobSort(q.Sort) {
		return nil, fmt.Errorf("%w: unknown job order %q", ErrInvalidFilter, q.Sort)
	}
	defer s.lockKey()()
	w := &JobWindow{Jobs: []*models.Job{}}
	read := 0
	for _, seg := range jobWindowSQL(q) {
		query, args := seg.query, seg.args
		if q.Limit > 0 {
			// One row more than the window tells whether another one follows.
			query += " LIMIT ?"
			args = append(args, q.Limit+1-read)
		}
		done, err := s.readJobSegment(ctx, q, seg.null, query, args, w, &read)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	return w, nil
}

// readJobSegment reads the rows of one segment of q into w, counting them in read;
// done reports that the window is full (w.More is then set).
func (s *SQLiteStore) readJobSegment(ctx context.Context, q JobQuery, null bool, query string, args []any, w *JobWindow, read *int) (done bool, err error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("store: query jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if q.Limit > 0 && *read == q.Limit {
			w.More = true
			return true, nil
		}
		var (
			id, data string
			value    any
		)
		if err := rows.Scan(&id, &value, &data); err != nil {
			return false, fmt.Errorf("store: scan job: %w", err)
		}
		*read++
		w.Last = rowJobKey(null, id, value)
		job, err := decode[models.Job](data)
		if err == nil {
			if err = s.openJob(job); errors.Is(err, ErrNoSecretBox) {
				return false, err
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
		return false, fmt.Errorf("store: iterate jobs: %w", err)
	}
	return false, nil
}

// rowJobKey builds the key of a row read by jobWindowSQL from its ID and value.
func rowJobKey(null bool, id string, value any) JobKey {
	k := JobKey{ID: id, Null: null}
	if null {
		return k
	}
	switch v := value.(type) {
	case string:
		k.Str = v
	case []byte:
		k.Str = string(v)
	case int64:
		k.Int = v
	case float64:
		k.Real = v
	}
	return k
}

// CountJobs counts the jobs matching the filters of q (its cursor, order and limit
// are ignored), from the jobs indexes without reading a job. Rows known to be
// unreadable (see CorruptRecords; every row is checked when the store opens) are
// left out, as QueryJobs skips them, so the count matches the jobs a client can
// page through.
func (s *SQLiteStore) CountJobs(ctx context.Context, q JobQuery) (int, error) {
	query, args := jobCountSQL(q)
	if ids := s.corruptIDs(tableJobs); len(ids) > 0 {
		list, err := json.Marshal(ids)
		if err != nil {
			return 0, fmt.Errorf("store: count jobs: %w", err)
		}
		// One JSON argument, whatever the number of rows.
		if strings.Contains(query, " WHERE ") {
			query += " AND id NOT IN (SELECT value FROM json_each(?))"
		} else {
			query += " WHERE id NOT IN (SELECT value FROM json_each(?))"
		}
		args = append(args, string(list))
	}
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
