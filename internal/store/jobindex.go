package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// jobIndexVersion is the version of jobIndexColumns. Rows whose index_version is
// older (every row after migration 0029) are filled again when the database opens
// (reindexJobs); raise it whenever jobIndexColumns computes something else.
const jobIndexVersion = 1

// JobSearchSeparator separates the fields of a job's search text and the names
// of its databases column (U+001F). A search or database filter containing it is
// not answered by the index (see JobQuery).
const JobSearchSeparator = "\x1f"

// jobIndex holds the columns of a job that the jobs list filters on (migration
// 0029), derived from the job's document by jobIndexColumns.
type jobIndex struct {
	// search is the lower-cased ID, name, database, selection search text and
	// known databases, separated by JobSearchSeparator: a search (lower-cased, and
	// without the separator) is a substring of it exactly when it is a substring
	// of one of them.
	search string
	// schedule is models.ScheduleFrequency of the cron expression.
	schedule string
	// databases is the separator, then every database the selection names and
	// every known database, each followed by the separator.
	databases string
	// discovers is 1 for an all or pattern selection, which may cover databases
	// it neither names nor knows (DatabaseSelection.Matches).
	discovers int
}

// jobIndexColumns returns the filter columns of j.
func jobIndexColumns(j *models.Job) jobIndex {
	sel := j.Selection()
	fields := append([]string{j.ID, j.Name, j.Database, sel.SearchText()}, j.KnownDatabases...)
	for i, f := range fields {
		fields[i] = strings.ToLower(f)
	}
	names := append(slices.Clone(sel.Databases), j.KnownDatabases...)
	slices.Sort(names)
	names = slices.Compact(names)
	databases := JobSearchSeparator
	for _, n := range names {
		databases += n + JobSearchSeparator
	}
	idx := jobIndex{
		search:    strings.Join(fields, JobSearchSeparator),
		schedule:  models.ScheduleFrequency(j.CronExpression),
		databases: databases,
	}
	if sel.Discovers() {
		idx.discovers = 1
	}
	return idx
}

// args returns the columns as the trailing arguments of upsertJobSQL and the
// other job writes: search_text, schedule, databases, discovers, index_version.
func (x jobIndex) args() []any {
	return []any{x.search, x.schedule, x.databases, x.discovers, jobIndexVersion}
}

// jobIndexSet assigns the columns of jobIndex.args in an UPDATE of jobs.
const jobIndexSet = "search_text = ?, schedule = ?, databases = ?, discovers = ?, index_version = ?"

// reindexJobs fills the filter columns of the jobs written by an older version of
// jobIndexColumns (after migration 0029, every job), in one transaction. A job
// whose document cannot be read keeps its old columns (no filter matches it, and
// the lists skip it anyway) and is tried again at the next start.
func (s *SQLiteStore) reindexJobs(ctx context.Context) error {
	type row struct{ id, data string }
	var stale []row
	err := func() error {
		rows, err := s.db.QueryContext(ctx, "SELECT id, data FROM jobs WHERE index_version < ?", jobIndexVersion)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.data); err != nil {
				return err
			}
			stale = append(stale, r)
		}
		return rows.Err()
	}()
	if err != nil {
		return fmt.Errorf("store: read the jobs to index: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	skipped := 0
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, prepErr := tx.PrepareContext(ctx, "UPDATE jobs SET "+jobIndexSet+" WHERE id = ?")
		if prepErr != nil {
			return prepErr
		}
		defer func() { _ = stmt.Close() }()
		for _, r := range stale {
			j, decErr := decode[models.Job](r.data)
			if decErr != nil {
				skipped++
				continue
			}
			if _, execErr := stmt.ExecContext(ctx, append(jobIndexColumns(j).args(), r.id)...); execErr != nil {
				return execErr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: index the jobs: %w", err)
	}
	s.logger.Info("indexed the jobs for the jobs list filters",
		slog.Int("jobs", len(stale)-skipped), slog.Int("unreadable", skipped))
	return nil
}

// Last statuses of JobQuery.LastStatus: the outcome of a job's newest backup (a
// multi-database job: its newest run that was not skipped), as migration 0029's
// job_last_outcomes view ranks them in jobs.last_status.
const (
	// JobLastFailed is a failed backup or run.
	JobLastFailed = "failed"
	// JobLastPartial is a multi-database run that backed up some databases.
	JobLastPartial = "partial"
	// JobLastCancelled is a cancelled backup or run.
	JobLastCancelled = "cancelled"
	// JobLastInProgress is a pending or running backup or run.
	JobLastInProgress = "in_progress"
	// JobLastCompleted is any other backup (completed, also pruned, deleted or
	// missing since) or a run that backed up every database.
	JobLastCompleted = "completed"
	// JobLastNever is a job without backups or runs (last_status NULL).
	JobLastNever = "never"
)

// jobLastStatusRank is the jobs.last_status of each last status but JobLastNever:
// the order of JobSortLastStatus, failures first.
var jobLastStatusRank = map[string]int64{
	JobLastFailed: 0, JobLastPartial: 1, JobLastCancelled: 2, JobLastInProgress: 3, JobLastCompleted: 4,
}

// JobLastStatusRank returns the rank of a last status in JobSortLastStatus order
// (JobKey.Int), and false for JobLastNever and unknown values.
func JobLastStatusRank(status string) (int64, bool) {
	r, ok := jobLastStatusRank[status]
	return r, ok
}

// JobStatusTime returns the JobKey.Time of a last status whose backup or run
// started at t, as jobs.last_status_at stores it.
func JobStatusTime(t time.Time) int64 { return timeKey(t) }

// HasForeignLastBackups reports whether a job of the connections in set has its
// newest backup on a connection outside set (a single-database job that moved).
// For such a job, the last status the jobs table keeps is not the one of the
// newest backup the caller may see, so a caller limited to set cannot filter or
// sort by it in SQL. An unlimited set has none.
func (s *SQLiteStore) HasForeignLastBackups(ctx context.Context, set auth.ConnectionSet) (bool, error) {
	if !set.Limited() {
		return false, nil
	}
	var c, last conditions
	c.addConnections("connection_id", set)
	last.addConnections("last_backup_connection", set)
	c.add("last_backup_connection IS NOT NULL AND NOT ("+strings.Join(last.clauses, " AND ")+")", last.args...)
	var found bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM jobs"+c.where()+")", c.args...).Scan(&found); err != nil { //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
		return false, fmt.Errorf("store: check the jobs' last backups: %w", err)
	}
	return found, nil
}

// ChangeStamp returns the number of writes to the tables the overview and the
// readiness report read (jobs, backups, job_runs, restores, restore_tests,
// rpo_breaches, job_database_joins), which triggers count (migration 0029). A
// cached result computed at one stamp is current while the stamp is unchanged.
func (s *SQLiteStore) ChangeStamp(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT n FROM store_changes WHERE id = 1").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: read the change stamp: %w", err)
	}
	return n, nil
}
