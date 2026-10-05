package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// BackupStats are aggregates over every backup record, computed in SQL.
type BackupStats struct {
	// Total counts every backup record.
	Total int
	// ByStatus counts the records of each status.
	ByStatus map[models.BackupStatus]int
	// FailedSince counts failed backups that started at or after the time passed to
	// BackupStats.
	FailedSince int
	// CompletedBytes sums the size of completed backups.
	CompletedBytes int64
	// Last is the newest backup (by start time, then ID), or nil when there is none.
	Last *models.BackupRecord
}

// Active counts the backups that are pending or in progress.
func (b *BackupStats) Active() int {
	return b.ByStatus[models.StatusPending] + b.ByStatus[models.StatusInProgress]
}

// RestoreStats are aggregates over every restore record, computed in SQL.
type RestoreStats struct {
	// Total counts every restore record.
	Total int
	// ByStatus counts the records of each status.
	ByStatus map[models.RestoreStatus]int
}

// Active counts the restores that are pending or in progress.
func (r *RestoreStats) Active() int {
	return r.ByStatus[models.RestoreStatusPending] + r.ByStatus[models.RestoreStatusInProgress]
}

// BackupStats returns counts by status, the number of failures that started at or
// after since, the size of completed backups and the newest backup, without reading
// every record.
func (s *SQLiteStore) BackupStats(ctx context.Context, since time.Time) (*BackupStats, error) {
	return s.BackupStatsIn(ctx, since, nil)
}

// BackupStatsIn is BackupStats over the backups taken from the connections in set
// (every backup when set is nil).
func (s *SQLiteStore) BackupStatsIn(ctx context.Context, since time.Time, set auth.ConnectionSet) (*BackupStats, error) {
	st := &BackupStats{ByStatus: map[models.BackupStatus]int{}}
	var c conditions
	c.addConnections("connection_id", set)
	query := `SELECT status, count(*), coalesce(sum(size_bytes), 0) FROM backups` + c.where() + ` GROUP BY status` //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	rows, err := s.db.QueryContext(ctx, query, c.args...)
	if err != nil {
		return nil, fmt.Errorf("store: backup stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			status string
			n      int
			bytes  int64
		)
		if err = rows.Scan(&status, &n, &bytes); err != nil {
			return nil, fmt.Errorf("store: scan backup stats: %w", err)
		}
		st.ByStatus[models.BackupStatus(status)] = n
		st.Total += n
		if models.BackupStatus(status) == models.StatusCompleted {
			st.CompletedBytes = bytes
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read backup stats: %w", err)
	}
	recent := conditions{clauses: []string{"status = ?", "started_at >= ?"}, args: []any{string(models.StatusFailed), timeKey(since)}}
	recent.addConnections("connection_id", set)
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM backups`+recent.where(), recent.args...).Scan(&st.FailedSince); err != nil {
		return nil, fmt.Errorf("store: count recent failures: %w", err)
	}
	// A newest backup that cannot be read is skipped (and reported): Last is the newest
	// readable one.
	if st.Last, err = firstReadable[models.BackupRecord](ctx, s, tableBackups,
		"SELECT id, data FROM backups"+c.where()+" ORDER BY started_at DESC, id DESC", c.args...); err != nil {
		return nil, err
	}
	return st, nil
}

// LatestJobBackups maps every job ID that has backups to its newest backup (by start
// time, then ID). A non-empty status only considers backups in that state. When the
// newest backup of a job cannot be read, it is skipped (and reported) and the job's
// newest readable backup is used instead.
func (s *SQLiteStore) LatestJobBackups(ctx context.Context, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	return s.LatestJobBackupsIn(ctx, status, nil)
}

// LatestJobBackupsIn is LatestJobBackups over the backups taken from the connections
// in set (every backup when set is nil): a backup's own connection decides, so a
// job that moved keeps its newest backup of set.
func (s *SQLiteStore) LatestJobBackupsIn(ctx context.Context, status models.BackupStatus, set auth.ConnectionSet) (map[string]*models.BackupRecord, error) {
	clause, connArgs := connectionClause("connection_id", set)
	bClause := strings.Replace(clause, "connection_id", "b.connection_id", 1)
	query := `SELECT b.id, b.job_id, b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != ''` + clause + ` GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest WHERE 1` + bClause
	args := append(slices.Clone(connArgs), connArgs...)
	if status != "" {
		query = `SELECT b.id, b.job_id, b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != '' AND status = ?` + clause + ` GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest WHERE b.status = ?` + bClause
		args = append(append(append([]any{string(status)}, connArgs...), string(status)), connArgs...)
	}
	out, skipped, err := s.latestJobRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for jobID := range skipped {
		if _, ok := out[jobID]; ok {
			continue
		}
		fallback := "SELECT id, data FROM backups WHERE job_id = ?"
		fargs := []any{jobID}
		if status != "" {
			fallback += " AND status = ?"
			fargs = append(fargs, string(status))
		}
		fallback += clause + " ORDER BY started_at DESC, id DESC"
		fargs = append(fargs, connArgs...)
		b, err := firstReadable[models.BackupRecord](ctx, s, tableBackups, fallback, fargs...)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out[jobID] = b
		}
	}
	return out, nil
}

// latestJobRows reads the newest backup rows per job selected by query (id, job_id,
// data). It returns the readable ones by job ID, and the jobs with a skipped row.
func (s *SQLiteStore) latestJobRows(ctx context.Context, query string, args ...any) (map[string]*models.BackupRecord, map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*models.BackupRecord{}
	skipped := map[string]bool{}
	for rows.Next() {
		var id, jobID, data string
		if err := rows.Scan(&id, &jobID, &data); err != nil {
			return nil, nil, fmt.Errorf("store: scan row: %w", err)
		}
		b, err := decode[models.BackupRecord](data)
		if err != nil {
			s.reportCorrupt(tableBackups, id, err)
			skipped[jobID] = true
			continue
		}
		s.clearCorrupt(tableBackups, id)
		// Records sharing the newest start time: the greatest ID wins, as in the lists.
		if prev, ok := out[jobID]; !ok || b.ID > prev.ID {
			out[jobID] = b
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate rows: %w", err)
	}
	return out, skipped, nil
}

// RestoreStats returns the number of restores and their counts by status.
func (s *SQLiteStore) RestoreStats(ctx context.Context) (*RestoreStats, error) {
	return s.RestoreStatsIn(ctx, nil)
}

// RestoreStatsIn is RestoreStats over the restores whose source and target
// connections are in set (every restore when set is nil).
func (s *SQLiteStore) RestoreStatsIn(ctx context.Context, set auth.ConnectionSet) (*RestoreStats, error) {
	st := &RestoreStats{ByStatus: map[models.RestoreStatus]int{}}
	var c conditions
	c.addConnections(restoreSourceSQL, set)
	c.addConnections(restoreTargetSQL, set)
	query := `SELECT r.status, count(*) FROM restores r` + c.where() + ` GROUP BY r.status` //nolint:gosec // G202: only constant clauses are joined; values are ? arguments.
	rows, err := s.db.QueryContext(ctx, query, c.args...)
	if err != nil {
		return nil, fmt.Errorf("store: restore stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err = rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: scan restore stats: %w", err)
		}
		st.ByStatus[models.RestoreStatus(status)] = n
		st.Total += n
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read restore stats: %w", err)
	}
	return st, nil
}

// LatestJobDatabaseBackups maps every database of job jobID with backups in status
// (any status when empty) to its newest such backup. Rows that cannot be read are
// skipped.
func (s *SQLiteStore) LatestJobDatabaseBackups(ctx context.Context, jobID string, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	// An empty status matches every status (status = '' never holds for a record).
	query := `SELECT b.id, b.data FROM backups b JOIN (
			SELECT database_name, max(started_at) AS latest FROM backups
			WHERE job_id = ? AND (? = '' OR status = ?) GROUP BY database_name
		) l ON b.database_name = l.database_name AND b.started_at = l.latest
		WHERE b.job_id = ? AND (? = '' OR b.status = ?) ORDER BY b.started_at DESC, b.id DESC`
	st := string(status)
	list, err := listRecords[models.BackupRecord](ctx, s, tableBackups, nil, query, jobID, st, st, jobID, st, st)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*models.BackupRecord, len(list))
	for _, b := range list {
		if _, seen := out[b.Database]; !seen {
			out[b.Database] = b
		}
	}
	return out, nil
}
