package store

import (
	"context"
	"fmt"
	"time"

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
	st := &BackupStats{ByStatus: map[models.BackupStatus]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*), coalesce(sum(size_bytes), 0) FROM backups GROUP BY status`)
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
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM backups WHERE status = ? AND started_at >= ?`,
		string(models.StatusFailed), timeKey(since)).Scan(&st.FailedSince); err != nil {
		return nil, fmt.Errorf("store: count recent failures: %w", err)
	}
	// A newest backup that cannot be read is skipped (and reported): Last is the newest
	// readable one.
	if st.Last, err = firstReadable[models.BackupRecord](ctx, s, tableBackups,
		"SELECT id, data FROM backups ORDER BY started_at DESC, id DESC"); err != nil {
		return nil, err
	}
	return st, nil
}

// LatestJobBackups maps every job ID that has backups to its newest backup (by start
// time, then ID). A non-empty status only considers backups in that state. When the
// newest backup of a job cannot be read, it is skipped (and reported) and the job's
// newest readable backup is used instead.
func (s *SQLiteStore) LatestJobBackups(ctx context.Context, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	query := `SELECT b.id, b.job_id, b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != '' GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest`
	var args []any
	if status != "" {
		query = `SELECT b.id, b.job_id, b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != '' AND status = ? GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest WHERE b.status = ?`
		args = []any{string(status), string(status)}
	}
	out, skipped, err := s.latestJobRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for jobID := range skipped {
		if _, ok := out[jobID]; ok {
			continue
		}
		fallback := "SELECT id, data FROM backups WHERE job_id = ? ORDER BY started_at DESC, id DESC"
		fargs := []any{jobID}
		if status != "" {
			fallback = "SELECT id, data FROM backups WHERE job_id = ? AND status = ? ORDER BY started_at DESC, id DESC"
			fargs = append(fargs, string(status))
		}
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
	st := &RestoreStats{ByStatus: map[models.RestoreStatus]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM restores GROUP BY status`)
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
