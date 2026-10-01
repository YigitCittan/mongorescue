package store

import (
	"context"
	"errors"
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
	st.Last, err = getRecord[models.BackupRecord](ctx, s.db, ErrNotFound, "SELECT data FROM backups ORDER BY started_at DESC, id DESC LIMIT 1")
	if errors.Is(err, ErrNotFound) {
		st.Last, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// LatestJobBackups maps every job ID that has backups to its newest backup (by start
// time, then ID). A non-empty status only considers backups in that state.
func (s *SQLiteStore) LatestJobBackups(ctx context.Context, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	query := `SELECT b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != '' GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest`
	var args []any
	if status != "" {
		query = `SELECT b.data FROM backups b JOIN (
			SELECT job_id, max(started_at) AS latest FROM backups WHERE job_id != '' AND status = ? GROUP BY job_id
		) l ON b.job_id = l.job_id AND b.started_at = l.latest WHERE b.status = ?`
		args = []any{string(status), string(status)}
	}
	list, err := listRecords[models.BackupRecord](ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*models.BackupRecord, len(list))
	for _, b := range list {
		// Records sharing the newest start time: the greatest ID wins, as in the lists.
		if prev, ok := out[b.JobID]; !ok || b.ID > prev.ID {
			out[b.JobID] = b
		}
	}
	return out, nil
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
