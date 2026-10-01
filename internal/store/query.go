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

// MaxFilterIDs is the largest number of IDs in BackupFilter.IDs.
const MaxFilterIDs = 200

// SortOrder orders filtered list results by start time.
type SortOrder string

// Sort orders of the filtered list queries.
const (
	// SortNewest lists the newest records first (the default).
	SortNewest SortOrder = "desc"
	// SortOldest lists the oldest records first.
	SortOldest SortOrder = "asc"
)

// BackupFilter selects, orders and pages backup records. Zero fields match
// everything; all set fields must match.
type BackupFilter struct {
	// IDs keeps the backups with exactly these IDs (at most MaxFilterIDs).
	IDs []string
	// Status keeps backups in this state.
	Status models.BackupStatus
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
	// From keeps backups started at or after this time.
	From time.Time
	// To keeps backups started before this time.
	To time.Time
	// Search keeps backups whose ID or database contains this text literally, ignoring
	// ASCII case.
	Search string
	// Sort orders by start time: SortNewest (also when empty) or SortOldest.
	Sort SortOrder
	// Limit is the page size, 1 to MaxListLimit; 0 returns every match.
	Limit int
	// Offset skips this many matches.
	Offset int
}

// RestoreFilter selects, orders and pages restore records. Zero fields match
// everything; all set fields must match.
type RestoreFilter struct {
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
	// Sort orders by start time: SortNewest (also when empty) or SortOldest.
	Sort SortOrder
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

// orderAndPage renders the ORDER BY and LIMIT/OFFSET clauses for column prefix p and
// appends their arguments to args. SQLite reads LIMIT -1 as no limit.
func orderAndPage(p string, sort SortOrder, limit, offset int, args []any) (string, []any) {
	order := " ORDER BY " + p + "started_at DESC, " + p + "id DESC"
	if sort == SortOldest {
		order = " ORDER BY " + p + "started_at ASC, " + p + "id ASC"
	}
	if limit == 0 && offset == 0 {
		return order, args
	}
	if limit == 0 {
		limit = -1
	}
	return order + " LIMIT ? OFFSET ?", append(args, limit, offset)
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
	if len(f.IDs) > 0 {
		args := make([]any, len(f.IDs))
		for i, id := range f.IDs {
			args[i] = id
		}
		// One constant "?" per ID; the IDs themselves are arguments.
		c.add("b.id IN (?"+strings.Repeat(", ?", len(f.IDs)-1)+")", args...)
	}
	if f.Status != "" {
		c.add("b.status = ?", string(f.Status))
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
	c.addTimeRange("b.", f.From, f.To)
	if f.Search != "" {
		// instr, unlike LIKE, has no wildcards: the search text is matched literally.
		q := strings.ToLower(f.Search)
		c.add("(instr(lower(b.id), ?) > 0 OR instr(lower(b.database_name), ?) > 0)", q, q)
	}
	return c
}

// selectBackupRowsSQL selects backups with the ID and start time of their newest retry.
const selectBackupRowsSQL = `SELECT b.data, r.id, r.started_at FROM backups b
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
	c := backupConditions(f)
	page, args := orderAndPage("b.", f.Sort, f.Limit, f.Offset, append([]any(nil), c.args...))
	query := selectBackupRowsSQL + c.where() + page //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query backups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := &BackupPage{Rows: make([]BackupRow, 0)}
	for rows.Next() {
		var (
			data      string
			retryID   sql.NullString
			retryTime sql.NullInt64
		)
		if scanErr := rows.Scan(&data, &retryID, &retryTime); scanErr != nil {
			return nil, fmt.Errorf("store: scan backup row: %w", scanErr)
		}
		rec, decErr := decode[models.BackupRecord](data)
		if decErr != nil {
			return nil, decErr
		}
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
	c := restoreConditions(f)
	page, args := orderAndPage("r.", f.Sort, f.Limit, f.Offset, append([]any(nil), c.args...))
	list, err := listRecords[models.RestoreRecord](ctx, s.db, "SELECT r.data FROM restores r"+c.where()+page, args...)
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
