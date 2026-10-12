package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// JobSortLastStatus orders by the last status (failed, partial, cancelled, in
	// progress, completed; see JobLastStatusRank), then by when that backup or run
	// started, the newest first; jobs that never ran come last in both directions.
	JobSortLastStatus JobSort = "last_status"
)

// JobKey is the position of a job in a JobSort order: its sort value and its ID.
// It is the cursor of keyset paging.
type JobKey struct {
	// Null marks a job without a value (JobSortNextRun: paused, or never
	// scheduled; JobSortLastStatus: never ran); such jobs come after the others in
	// both directions.
	Null bool `json:"n,omitempty"`
	// Str, Int and Real hold the value: the name, the creation time in Unix
	// nanoseconds or the rank of the last status, or the next run as a Julian day
	// number (SQLite's julianday). The fields an order does not use stay zero.
	Str  string  `json:"k,omitempty"`
	Int  int64   `json:"i,omitempty"`
	Real float64 `json:"r,omitempty"`
	// Time is when the last status's backup or run started, in Unix nanoseconds
	// (JobSortLastStatus); it orders in reverse, the newest first.
	Time int64 `json:"t,omitempty"`
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
	c := cmp.Or(strings.Compare(a.Str, b.Str), cmp.Compare(a.Int, b.Int), cmp.Compare(b.Time, a.Time), cmp.Compare(a.Real, b.Real),
		strings.Compare(a.ID, b.ID))
	if desc {
		return -c
	}
	return c
}

// JobKeyOf returns the key of job j in order sort, as QueryJobs computes it. The
// last status is not part of a job's document: for JobSortLastStatus, the key
// has the ID only (see JobLastStatusRank for the rest).
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
	case JobSortLastStatus:
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
	// Search keeps the jobs whose ID, name, database, selection (mode, names and
	// patterns) or a known database contains it, ignoring case (the jobs list's
	// q). It must not contain JobSearchSeparator.
	Search string
	// Database keeps the jobs that back it up (models.Job.Covers). It must not
	// contain JobSearchSeparator.
	Database string
	// Schedule keeps the jobs whose cron expression has this frequency
	// (models.ScheduleFrequency).
	Schedule string
	// LastStatus keeps the jobs with this last status: JobLastNever or one of
	// JobLastStatusRank's.
	LastStatus string

	// databaseIDs are the jobs with an all or pattern selection whose patterns
	// match Database without naming or knowing it (resolveDatabase).
	databaseIDs []string
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
	if q.Search != "" {
		c.add("instr(search_text, ?) > 0", strings.ToLower(q.Search))
	}
	if q.Schedule != "" {
		c.add("schedule = ?", q.Schedule)
	}
	if q.Database != "" {
		name := JobSearchSeparator + q.Database + JobSearchSeparator
		if len(q.databaseIDs) == 0 {
			c.add("instr(databases, ?) > 0", name)
		} else {
			ids, _ := json.Marshal(q.databaseIDs)
			// One JSON argument, whatever the number of jobs.
			c.add("(instr(databases, ?) > 0 OR id IN (SELECT value FROM json_each(?)))", name, string(ids))
		}
	}
	switch rank, ok := jobLastStatusRank[q.LastStatus]; {
	case q.LastStatus == JobLastNever:
		c.add("last_status IS NULL")
	case ok:
		c.add("last_status = ?", rank)
	}
	return c
}

// clone returns a copy of c that can be extended without changing c.
func (c *conditions) clone() *conditions {
	return &conditions{clauses: slices.Clone(c.clauses), args: slices.Clone(c.args)}
}

// checkJobQuery returns an ErrInvalidFilter error for a limit, an order or a
// filter value out of range.
func checkJobQuery(q JobQuery) error {
	switch _, ranked := jobLastStatusRank[q.LastStatus]; {
	case q.Limit < 0 || q.Limit > MaxJobWindow:
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidFilter, MaxJobWindow)
	case !validJobSort(q.Sort):
		return fmt.Errorf("%w: unknown job order %q", ErrInvalidFilter, q.Sort)
	case q.LastStatus != "" && q.LastStatus != JobLastNever && !ranked:
		return fmt.Errorf("%w: unknown last status %q", ErrInvalidFilter, q.LastStatus)
	case strings.Contains(q.Search, JobSearchSeparator) || strings.Contains(q.Database, JobSearchSeparator):
		return fmt.Errorf("%w: the search and the database must not contain U+001F", ErrInvalidFilter)
	}
	return nil
}

// resolveDatabase sets q.databaseIDs for a Database filter: the jobs of q's other
// filters whose all or pattern selection matches the database without naming or
// knowing it, evaluated with models.Job.Covers on their documents (few jobs
// discover their databases; the others match on the databases column alone).
func (s *SQLiteStore) resolveDatabase(ctx context.Context, q *JobQuery) error {
	if q.Database == "" {
		return nil
	}
	other := *q
	other.Database = ""
	c := jobConditions(other)
	c.add("discovers = 1 AND instr(databases, ?) = 0", JobSearchSeparator+q.Database+JobSearchSeparator)
	// The partial index holds the discovering jobs only; the other filters' indexes
	// would read every job.
	rows, err := s.db.QueryContext(ctx, "SELECT id, data FROM jobs INDEXED BY jobs_discovering"+c.where(), c.args...) //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	if err != nil {
		return fmt.Errorf("store: query jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	q.databaseIDs = nil
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return fmt.Errorf("store: scan job: %w", err)
		}
		if j, decErr := decode[models.Job](data); decErr == nil && j.Covers(q.Database) {
			q.databaseIDs = append(q.databaseIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate jobs: %w", err)
	}
	return nil
}

// jobSegment is one query of a window: the rows with a sort value, or (null) those
// without one, in order.
type jobSegment struct {
	query string
	args  []any
	null  bool
	// limitInside marks a query that ends with its LIMIT placeholder (a window
	// read in two steps); the others take " LIMIT ?" appended.
	limitInside bool
}

// validJobSort reports whether sort is an order of QueryJobs ("" is JobSortName).
func validJobSort(sort JobSort) bool {
	switch sort {
	case "", JobSortName, JobSortCreatedAt, JobSortNextRun, JobSortLastStatus:
		return true
	}
	return false
}

// lastStatusSetSQL holds for the rows with a last status (JobSortLastStatus),
// indexed by jobs_by_last_status and its connection and state variants.
const lastStatusSetSQL = "last_status IS NOT NULL"

// jobWindowSQL returns the queries of a QueryJobs window, run in turn: one for the
// orders whose rows all have a value, and for JobSortNextRun and
// JobSortLastStatus the rows with a value, then those without (never before the
// cursor's segment). Every query reads "id, value, time, data" in the order of an
// index; the cursor and the order are constant SQL, the values arguments.
func jobWindowSQL(q JobQuery) []jobSegment {
	dir, op, rev := "", ">", " DESC"
	if q.Desc {
		dir, op, rev = " DESC", "<", ""
	}
	base := jobConditions(q)
	// A window of the filters on the jobs' search, database, schedule or last
	// status columns is read in two steps (see below).
	deferred := q.Limit > 0 && (q.Search != "" || q.Database != "" || q.Schedule != "" || q.LastStatus != "")
	var segs []jobSegment
	value := func(expr string, null bool) {
		c := base.clone()
		set := ""
		switch q.Sort {
		case JobSortNextRun:
			set = nextRunSetSQL
		case JobSortLastStatus:
			set = lastStatusSetSQL
		}
		switch {
		case null && q.Sort == JobSortLastStatus:
			// Both are NULL together; with both, the index reads them in ID order.
			c.add("last_status IS NULL AND last_status_at IS NULL")
		case null:
			c.add("NOT (" + set + ")")
		case set != "":
			c.add(set)
		}
		a := q.After
		switch {
		case a == nil || a.Null != null:
		case null:
			c.add("id "+op+" ?", a.ID)
		case q.Sort == JobSortLastStatus:
			// The rank in the order's direction (a range of the index), without the
			// rows of the cursor's rank up to the cursor: the start time runs against
			// the direction, the ID with it. An OR of the two ranges would cost the
			// index its order.
			from, before, upTo := ">=", ">", "<="
			if q.Desc {
				from, before, upTo = "<=", "<", ">="
			}
			c.add("last_status "+from+" ? AND NOT (last_status = ? AND (last_status_at "+before+" ? OR (last_status_at = ? AND id "+upTo+" ?)))",
				a.Int, a.Int, a.Time, a.Time, a.ID)
		default:
			c.add("("+expr+", id) "+op+" (?, ?)", afterValue(q.Sort, *a), a.ID)
		}
		order := expr + dir + ", id" + dir
		col, timeCol := expr, "0"
		if q.Sort == JobSortLastStatus {
			order = "last_status" + dir + ", last_status_at" + rev + ", id" + dir
			timeCol = "last_status_at"
		}
		if null {
			order, col, timeCol = "id"+dir, "NULL", "0"
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
		if deferred && q.Sort != JobSortNextRun {
			// The window's IDs first, from the filter columns (the covering
			// jobs_filter index, or the order's index), then the documents of the
			// window only: a filter the order's index cannot serve never reads, nor
			// sorts, the documents of every match.
			segs = append(segs, jobSegment{query: "SELECT id, " + col + ", " + timeCol + ", data FROM jobs WHERE id IN (SELECT id FROM jobs" + c.where() +
				" ORDER BY " + order + " LIMIT ?) ORDER BY " + order, args: c.args, null: null, limitInside: true})
			return
		}
		segs = append(segs, jobSegment{query: "SELECT id, " + col + ", " + timeCol + ", data FROM " + from + c.where() + " ORDER BY " + order, args: c.args, null: null})
	}
	switch q.Sort {
	case JobSortCreatedAt:
		value("created_at", false)
	case JobSortNextRun, JobSortLastStatus:
		expr := nextRunSQL
		if q.Sort == JobSortLastStatus {
			expr = "last_status"
		}
		if q.After == nil || !q.After.Null {
			value(expr, false)
		}
		value(expr, true)
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
	from := "jobs"
	if q.Search != "" || q.Database != "" || q.Schedule != "" || q.LastStatus != "" {
		// jobs_filter holds every column of the filters: the count never reads a
		// document, also where the planner's statistics would prefer the table.
		from = "jobs INDEXED BY jobs_filter"
	}
	return "SELECT count(*) FROM " + from + c.where(), c.args
}

// QueryJobs reads the jobs matching q after its cursor, in its order (from the jobs
// indexes of that order), at most q.Limit of them. It returns an ErrInvalidFilter
// error for a limit, an order or a filter out of range.
func (s *SQLiteStore) QueryJobs(ctx context.Context, q JobQuery) (*JobWindow, error) {
	if err := checkJobQuery(q); err != nil {
		return nil, err
	}
	if err := s.resolveDatabase(ctx, &q); err != nil {
		return nil, err
	}
	defer s.lockKey()()
	w := &JobWindow{Jobs: []*models.Job{}}
	read := 0
	for _, seg := range jobWindowSQL(q) {
		query, args := seg.query, seg.args
		if q.Limit > 0 {
			// One row more than the window tells whether another one follows.
			if !seg.limitInside {
				query += " LIMIT ?"
			}
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
			at       int64
		)
		if err := rows.Scan(&id, &value, &at, &data); err != nil {
			return false, fmt.Errorf("store: scan job: %w", err)
		}
		*read++
		w.Last = rowJobKey(null, id, value)
		w.Last.Time = at
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
	q.Limit, q.Sort, q.After = 0, "", nil
	if err := checkJobQuery(q); err != nil {
		return 0, err
	}
	if err := s.resolveDatabase(ctx, &q); err != nil {
		return 0, err
	}
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
