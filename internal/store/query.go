package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ErrInvalidFilter is returned by the filtered list queries when a filter value is out
// of range (a limit above MaxListLimit, a negative offset, an unknown sort order, or a
// time range that ends before it starts).
var ErrInvalidFilter = errors.New("store: invalid list filter")

// MaxListLimit is the largest page size of QueryBackupRecords and QueryRestoreRecords.
const MaxListLimit = 200

// MaxFilterIDs is the largest number of IDs in BackupFilter.IDs and RestoreFilter.IDs.
const MaxFilterIDs = 200

// SortOrder orders filtered list results by start time.
type SortOrder string

// Sort orders of the filtered list queries.
const (
	// SortNewest lists the newest records first (the default); with a SortKey other
	// than SortStartedAt it sorts descending.
	SortNewest SortOrder = "desc"
	// SortOldest lists the oldest records first; with a SortKey other than
	// SortStartedAt it sorts ascending.
	SortOldest SortOrder = "asc"
)

// SortKey names the column a filtered list query orders by. Only the keys below are
// accepted: each maps to a constant SQL expression, so ORDER BY is never built from
// input.
type SortKey string

// Sort keys of the filtered list queries.
const (
	// SortStartedAt orders by start time (the default).
	SortStartedAt SortKey = "started_at"
	// SortDuration orders by run time; records without one sort as zero.
	SortDuration SortKey = "duration"
	// SortSize orders backups by archive size (backups only).
	SortSize SortKey = "size"
	// SortDatabase orders by database name (restores: the target database).
	SortDatabase SortKey = "database"
	// SortStatus orders by status name.
	SortStatus SortKey = "status"
)

// BackupSortKeys and RestoreSortKeys are the accepted SortBy values of BackupFilter
// and RestoreFilter, in documentation order.
var (
	BackupSortKeys  = []SortKey{SortStartedAt, SortDuration, SortSize, SortDatabase, SortStatus}
	RestoreSortKeys = []SortKey{SortStartedAt, SortDuration, SortDatabase, SortStatus}
)

// backupSortSQL and restoreSortSQL map the sort keys to their constant ORDER BY
// expressions.
var (
	backupSortSQL = map[SortKey]string{
		SortStartedAt: "b.started_at",
		SortDuration:  "coalesce(json_extract(b.data, '$.duration_seconds'), 0)",
		SortSize:      "b.size_bytes",
		SortDatabase:  "b.database_name",
		SortStatus:    "b.status",
	}
	restoreSortSQL = map[SortKey]string{
		SortStartedAt: "r.started_at",
		SortDuration:  "coalesce(json_extract(r.data, '$.duration_seconds'), 0)",
		SortDatabase:  "r.target_database",
		SortStatus:    "r.status",
	}
)

// BackupFilter selects, orders and pages backup records. Zero fields match
// everything; all set fields must match.
type BackupFilter struct {
	// IDs keeps the backups with exactly these IDs (at most MaxFilterIDs).
	IDs []string
	// Status keeps backups in this state.
	Status models.BackupStatus
	// ExcludeStatuses drops backups in these states (the backup lists hide deleted
	// and purged backups unless asked for them).
	ExcludeStatuses []models.BackupStatus
	// Database keeps backups of exactly this database.
	Database string
	// ConnectionID keeps backups taken from this connection.
	ConnectionID string
	// JobID keeps backups taken by this scheduled job.
	JobID string
	// Trigger keeps backups started this way. Records written before triggers existed
	// match their effective trigger (see models.BackupRecord.EffectiveTrigger).
	Trigger models.BackupTrigger
	// RetryOf keeps the retries of this backup.
	RetryOf string
	// RunID keeps the backups of this job run.
	RunID string
	// From keeps backups started at or after this time.
	From time.Time
	// To keeps backups started before this time.
	To time.Time
	// Search keeps backups whose ID or database contains this text literally, ignoring
	// ASCII case.
	Search string
	// Sort is the direction: SortNewest (descending, also when empty) or SortOldest.
	Sort SortOrder
	// SortBy is the column ordered by (one of BackupSortKeys); empty is SortStartedAt.
	// Ties are broken by start time and ID, newest first.
	SortBy SortKey
	// Limit is the page size, 1 to MaxListLimit; 0 returns every match.
	Limit int
	// Offset skips this many matches.
	Offset int
}

// RestoreFilter selects, orders and pages restore records. Zero fields match
// everything; all set fields must match.
type RestoreFilter struct {
	// IDs keeps the restores with exactly these IDs (at most MaxFilterIDs).
	IDs []string
	// Status keeps restores in this state.
	Status models.RestoreStatus
	// BackupID keeps restores of this backup.
	BackupID string
	// TargetDatabase keeps restores into exactly this database.
	TargetDatabase string
	// From keeps restores started at or after this time.
	From time.Time
	// To keeps restores started before this time.
	To time.Time
	// Search keeps restores whose ID, source or target database contains this text
	// literally, ignoring ASCII case.
	Search string
	// Sort is the direction: SortNewest (descending, also when empty) or SortOldest.
	Sort SortOrder
	// SortBy is the column ordered by (one of RestoreSortKeys); empty is
	// SortStartedAt. Ties are broken by start time and ID, newest first.
	SortBy SortKey
	// Limit is the page size, 1 to MaxListLimit; 0 returns every match.
	Limit int
	// Offset skips this many matches.
	Offset int
}

// RetryRef names the newest retry of a backup.
type RetryRef struct {
	// ID is the retry's backup ID.
	ID string `json:"id"`
	// StartedAt is when the retry started.
	StartedAt time.Time `json:"started_at"`
}

// BackupRow is one result of QueryBackupRecords.
type BackupRow struct {
	// Record is the backup record.
	Record *models.BackupRecord
	// RetriedBy is the newest backup retrying Record, or nil.
	RetriedBy *RetryRef
}

// BackupPage is a page of QueryBackupRecords.
type BackupPage struct {
	// Rows are the matching records of the page, in the requested order.
	Rows []BackupRow
	// Total counts every match, ignoring Limit and Offset.
	Total int
}

// RestorePage is a page of QueryRestoreRecords.
type RestorePage struct {
	// Records are the matching records of the page, in the requested order.
	Records []*models.RestoreRecord
	// Total counts every match, ignoring Limit and Offset.
	Total int
}

// conditions collects the WHERE clause of a list query. Clauses are constant SQL
// with ? placeholders; filter values only ever travel as arguments.
type conditions struct {
	clauses []string
	args    []any
}

// add appends clause and its arguments.
func (c *conditions) add(clause string, args ...any) {
	c.clauses = append(c.clauses, clause)
	c.args = append(c.args, args...)
}

// where renders the WHERE clause, or "" when there are no conditions.
func (c *conditions) where() string {
	if len(c.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(c.clauses, " AND ")
}

// validatePage checks the shared paging and range fields of a filter.
func validatePage(sort SortOrder, limit, offset int, from, to time.Time) error {
	switch {
	case sort != "" && sort != SortNewest && sort != SortOldest:
		return fmt.Errorf("%w: sort must be %q or %q", ErrInvalidFilter, SortNewest, SortOldest)
	case limit < 0 || limit > MaxListLimit:
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidFilter, MaxListLimit)
	case offset < 0:
		return fmt.Errorf("%w: offset must not be negative", ErrInvalidFilter)
	case !from.IsZero() && !to.IsZero() && to.Before(from):
		return fmt.Errorf("%w: to must not be before from", ErrInvalidFilter)
	}
	return nil
}

// sortExpr returns the constant ORDER BY expression of key in columns (SortStartedAt
// when key is empty), or an ErrInvalidFilter error for a key the table has not.
func sortExpr(columns map[SortKey]string, accepted []SortKey, key SortKey) (string, error) {
	if key == "" {
		key = SortStartedAt
	}
	expr, ok := columns[key]
	if !ok {
		names := make([]string, len(accepted))
		for i, k := range accepted {
			names[i] = string(k)
		}
		return "", fmt.Errorf("%w: sort column must be one of %s", ErrInvalidFilter, strings.Join(names, ", "))
	}
	return expr, nil
}

// orderAndPage renders the ORDER BY and LIMIT/OFFSET clauses for column prefix p and
// appends their arguments to args. expr is a constant sort expression from sortExpr;
// other columns than the start time are tie-broken by start time and ID, newest
// first. SQLite reads LIMIT -1 as no limit.
func orderAndPage(p, expr string, sort SortOrder, limit, offset int, args []any) (string, []any) {
	dir := " DESC"
	if sort == SortOldest {
		dir = " ASC"
	}
	order := " ORDER BY " + expr + dir + ", " + p + "id" + dir
	if expr != p+"started_at" {
		order = " ORDER BY " + expr + dir + ", " + p + "started_at DESC, " + p + "id DESC"
	}
	if limit == 0 && offset == 0 {
		return order, args
	}
	if limit == 0 {
		limit = -1
	}
	return order + " LIMIT ? OFFSET ?", append(args, limit, offset)
}

// addIDs keeps the rows (of the table aliased p) whose id is one of ids; no IDs adds
// no condition.
func (c *conditions) addIDs(p string, ids []string) {
	if len(ids) == 0 {
		return
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// One constant "?" per ID; the IDs themselves are arguments.
	c.add(p+"id IN (?"+strings.Repeat(", ?", len(ids)-1)+")", args...)
}

// addTimeRange adds the started_at range of a filter.
func (c *conditions) addTimeRange(p string, from, to time.Time) {
	if !from.IsZero() {
		c.add(p+"started_at >= ?", timeKey(from))
	}
	if !to.IsZero() {
		c.add(p+"started_at < ?", timeKey(to))
	}
}

// effectiveTriggerSQL is a backup's trigger, falling back like
// models.BackupRecord.EffectiveTrigger for records written before triggers existed.
const effectiveTriggerSQL = `coalesce(nullif(json_extract(b.data, '$.trigger'), ''),
	CASE WHEN b.job_id != '' THEN 'scheduled' ELSE 'manual' END)`

// backupConditions renders f as the WHERE clause of a query over "backups b".
func backupConditions(f BackupFilter) conditions {
	var c conditions
	c.addIDs("b.", f.IDs)
	if f.Status != "" {
		c.add("b.status = ?", string(f.Status))
	}
	for _, st := range f.ExcludeStatuses {
		c.add("b.status != ?", string(st))
	}
	if f.Database != "" {
		c.add("b.database_name = ?", f.Database)
	}
	if f.ConnectionID != "" {
		c.add("b.connection_id = ?", f.ConnectionID)
	}
	if f.JobID != "" {
		c.add("b.job_id = ?", f.JobID)
	}
	if f.Trigger != "" {
		c.add(effectiveTriggerSQL+" = ?", string(f.Trigger))
	}
	if f.RetryOf != "" {
		c.add("b.retry_of = ?", f.RetryOf)
	}
	if f.RunID != "" {
		c.add("b.run_id = ?", f.RunID)
	}
	c.addTimeRange("b.", f.From, f.To)
	if f.Search != "" {
		// instr, unlike LIKE, has no wildcards: the search text is matched literally.
		q := strings.ToLower(f.Search)
		c.add("(instr(lower(b.id), ?) > 0 OR instr(lower(b.database_name), ?) > 0)", q, q)
	}
	return c
}

// selectBackupRowsSQL selects backups with the ID and start time of their newest retry.
const selectBackupRowsSQL = `SELECT b.id, b.data, r.id, r.started_at FROM backups b
	LEFT JOIN backups r ON r.id = (SELECT x.id FROM backups x WHERE x.retry_of = b.id
		ORDER BY x.started_at DESC, x.id DESC LIMIT 1)`

// QueryBackupRecords returns the backup records matching f, ordered and paged as f
// asks, each with its newest retry, and the number of all matches. It returns an
// ErrInvalidFilter error for out-of-range paging or time fields.
func (s *SQLiteStore) QueryBackupRecords(ctx context.Context, f BackupFilter) (*BackupPage, error) {
	if err := validatePage(f.Sort, f.Limit, f.Offset, f.From, f.To); err != nil {
		return nil, err
	}
	if len(f.IDs) > MaxFilterIDs {
		return nil, fmt.Errorf("%w: id takes at most %d IDs", ErrInvalidFilter, MaxFilterIDs)
	}
	expr, err := sortExpr(backupSortSQL, BackupSortKeys, f.SortBy)
	if err != nil {
		return nil, err
	}
	c := backupConditions(f)
	page, args := orderAndPage("b.", expr, f.Sort, f.Limit, f.Offset, append([]any(nil), c.args...))
	query := selectBackupRowsSQL + c.where() + page //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query backups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := &BackupPage{Rows: make([]BackupRow, 0)}
	for rows.Next() {
		var (
			id, data  string
			retryID   sql.NullString
			retryTime sql.NullInt64
		)
		if scanErr := rows.Scan(&id, &data, &retryID, &retryTime); scanErr != nil {
			return nil, fmt.Errorf("store: scan backup row: %w", scanErr)
		}
		rec, decErr := decode[models.BackupRecord](data)
		if decErr != nil {
			// Skipped like in every list; the page is one row short.
			s.reportCorrupt(tableBackups, id, decErr)
			continue
		}
		s.clearCorrupt(tableBackups, id)
		row := BackupRow{Record: rec}
		if retryID.Valid {
			row.RetriedBy = &RetryRef{ID: retryID.String, StartedAt: time.Unix(0, retryTime.Int64).UTC()}
		}
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read backup rows: %w", err)
	}
	if out.Total, err = s.total(ctx, "SELECT count(*) FROM backups b", c, f.Limit, f.Offset, len(out.Rows)); err != nil {
		return nil, err
	}
	return out, nil
}

// restoreConditions renders f as the WHERE clause of a query over "restores r".
func restoreConditions(f RestoreFilter) conditions {
	var c conditions
	c.addIDs("r.", f.IDs)
	if f.Status != "" {
		c.add("r.status = ?", string(f.Status))
	}
	if f.BackupID != "" {
		c.add("r.backup_id = ?", f.BackupID)
	}
	if f.TargetDatabase != "" {
		c.add("r.target_database = ?", f.TargetDatabase)
	}
	c.addTimeRange("r.", f.From, f.To)
	if f.Search != "" {
		q := strings.ToLower(f.Search)
		c.add("(instr(lower(r.id), ?) > 0 OR instr(lower(r.source_database), ?) > 0 OR instr(lower(r.target_database), ?) > 0)", q, q, q)
	}
	return c
}

// QueryRestoreRecords returns the restore records matching f, ordered and paged as f
// asks, and the number of all matches. It returns an ErrInvalidFilter error for
// out-of-range paging or time fields.
func (s *SQLiteStore) QueryRestoreRecords(ctx context.Context, f RestoreFilter) (*RestorePage, error) {
	if err := validatePage(f.Sort, f.Limit, f.Offset, f.From, f.To); err != nil {
		return nil, err
	}
	if len(f.IDs) > MaxFilterIDs {
		return nil, fmt.Errorf("%w: id takes at most %d IDs", ErrInvalidFilter, MaxFilterIDs)
	}
	expr, err := sortExpr(restoreSortSQL, RestoreSortKeys, f.SortBy)
	if err != nil {
		return nil, err
	}
	c := restoreConditions(f)
	page, args := orderAndPage("r.", expr, f.Sort, f.Limit, f.Offset, append([]any(nil), c.args...))
	list, err := listRecords[models.RestoreRecord](ctx, s, tableRestores, nil, "SELECT r.id, r.data FROM restores r"+c.where()+page, args...)
	if err != nil {
		return nil, err
	}
	out := &RestorePage{Records: list}
	if out.Total, err = s.total(ctx, "SELECT count(*) FROM restores r", c, f.Limit, f.Offset, len(list)); err != nil {
		return nil, err
	}
	return out, nil
}

// total counts the matches of a list query. An unpaged query needs no count: every
// match was returned.
func (s *SQLiteStore) total(ctx context.Context, count string, c conditions, limit, offset, n int) (int, error) {
	if limit == 0 && offset == 0 {
		return n, nil
	}
	var total int
	if err := s.db.QueryRowContext(ctx, count+c.where(), c.args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return total, nil
}

// ListBackupDatabases returns the distinct database names of all backup records,
// sorted.
func (s *SQLiteStore) ListBackupDatabases(ctx context.Context) ([]string, error) {
	return s.distinct(ctx, "SELECT DISTINCT database_name FROM backups WHERE database_name != '' ORDER BY database_name")
}

// ListRestoreDatabases returns the distinct target database names of all restore
// records, sorted.
func (s *SQLiteStore) ListRestoreDatabases(ctx context.Context) ([]string, error) {
	return s.distinct(ctx, "SELECT DISTINCT target_database FROM restores WHERE target_database != '' ORDER BY target_database")
}

// distinct returns the strings of the single column query selects; never nil.
func (s *SQLiteStore) distinct(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read rows: %w", err)
	}
	return out, nil
}
