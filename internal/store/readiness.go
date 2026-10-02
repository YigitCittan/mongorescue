package store

import (
	"context"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ListRPOBreaches returns every recorded RPO breach, ordered by job and database.
func (s *SQLiteStore) ListRPOBreaches(ctx context.Context) ([]models.RPOBreach, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT job_id, database_name, since FROM rpo_breaches ORDER BY job_id, database_name")
	if err != nil {
		return nil, fmt.Errorf("store: list rpo breaches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []models.RPOBreach{}
	for rows.Next() {
		var b models.RPOBreach
		var since int64
		if err := rows.Scan(&b.JobID, &b.Database, &since); err != nil {
			return nil, fmt.Errorf("store: scan rpo breach: %w", err)
		}
		b.Since = time.Unix(0, since).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rpo breaches: %w", err)
	}
	return out, nil
}

// AddRPOBreach records b unless a breach of the same job and database is already
// recorded, and reports whether it was added.
func (s *SQLiteStore) AddRPOBreach(ctx context.Context, b models.RPOBreach) (bool, error) {
	if b.JobID == "" {
		return false, fmt.Errorf("%w: rpo breach without a job", ErrInvalidRecord)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO rpo_breaches (job_id, database_name, since) VALUES (?, ?, ?)
		ON CONFLICT (job_id, database_name) DO NOTHING`, b.JobID, b.Database, timeKey(b.Since))
	if err != nil {
		return false, fmt.Errorf("store: add rpo breach: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: add rpo breach: %w", err)
	}
	return n > 0, nil
}

// DeleteRPOBreach removes the breach of job jobID and database and reports whether
// one was recorded.
func (s *SQLiteStore) DeleteRPOBreach(ctx context.Context, jobID, database string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM rpo_breaches WHERE job_id = ? AND database_name = ?", jobID, database)
	if err != nil {
		return false, fmt.Errorf("store: delete rpo breach: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete rpo breach: %w", err)
	}
	return n > 0, nil
}

// LatestVerifiedJobDatabaseBackups returns, per database, the newest completed
// backup of job jobID whose stored archive passed verification (verification "ok").
// Databases without one are absent.
func (s *SQLiteStore) LatestVerifiedJobDatabaseBackups(ctx context.Context, jobID string) (map[string]*models.BackupRecord, error) {
	const verified = `job_id = ? AND status = ? AND json_extract(data, '$.verification') = ?`
	query := `SELECT b.id, b.data FROM backups b JOIN (
			SELECT database_name, max(started_at) AS latest FROM backups WHERE ` + verified + ` GROUP BY database_name
		) l ON b.database_name = l.database_name AND b.started_at = l.latest
		WHERE b.` + verified + ` ORDER BY b.started_at DESC, b.id DESC`
	st, ok := string(models.StatusCompleted), string(models.VerificationOK)
	list, err := listRecords[models.BackupRecord](ctx, s, tableBackups, nil, query, jobID, st, ok, jobID, st, ok)
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

// LatestCompletedRestores returns the newest completed restore of every source
// connection and database that restored the whole database for real: dry runs and
// restores of selected collections are left out, as they say little about how long
// recovering the database takes.
func (s *SQLiteStore) LatestCompletedRestores(ctx context.Context) ([]*models.RestoreRecord, error) {
	query := `SELECT id, data FROM (
			SELECT id, data, ROW_NUMBER() OVER (
				PARTITION BY source_database, coalesce(json_extract(data, '$.source_connection_id'), '')
				ORDER BY started_at DESC, id DESC) AS rn
			FROM restores
			WHERE status = ? AND coalesce(json_extract(data, '$.dry_run'), 0) = 0
				AND coalesce(json_array_length(data, '$.selected_collections'), 0) = 0
		) WHERE rn = 1 ORDER BY id`
	return listRecords[models.RestoreRecord](ctx, s, tableRestores, nil, query, string(models.RestoreStatusCompleted))
}
