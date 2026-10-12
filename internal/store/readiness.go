package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
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

// JobDatabaseJoins returns, per job and database, when a database that joined the
// job after its first run (see UpdateJobKnownDatabases) was first counted among
// its known databases. Databases known from the start have no entry.
func (s *SQLiteStore) JobDatabaseJoins(ctx context.Context) (map[string]map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT job_id, database_name, joined_at FROM job_database_joins")
	if err != nil {
		return nil, fmt.Errorf("store: list database joins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]time.Time{}
	for rows.Next() {
		var job, db string
		var at int64
		if err := rows.Scan(&job, &db, &at); err != nil {
			return nil, fmt.Errorf("store: scan database join: %w", err)
		}
		if out[job] == nil {
			out[job] = map[string]time.Time{}
		}
		out[job][db] = time.Unix(0, at).UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate database joins: %w", err)
	}
	return out, nil
}

// LatestJobDatabaseBackupsAll returns, for every job and database, the newest
// completed backup of that job and database, in one query; with verified, the
// newest completed one whose stored archive passed verification ("ok").
func (s *SQLiteStore) LatestJobDatabaseBackupsAll(ctx context.Context, verified bool) (map[string]map[string]*models.BackupRecord, error) {
	return s.LatestJobDatabaseBackupsAllIn(ctx, verified, nil)
}

// LatestJobDatabaseBackupsAllIn is LatestJobDatabaseBackupsAll over the backups
// taken from the connections in set (every backup when set is nil): a backup's own
// connection decides, so a job that moved keeps the newest backup of set.
func (s *SQLiteStore) LatestJobDatabaseBackupsAllIn(ctx context.Context, verified bool, set auth.ConnectionSet) (map[string]map[string]*models.BackupRecord, error) {
	clause, connArgs := connectionClause("connection_id", set)
	// cond selects the rows of the table aliased t.
	cond := func(t string) string {
		c := t + `.job_id != '' AND ` + t + `.status = ?`
		if verified {
			c += ` AND json_extract(` + t + `.data, '$.verification') = ?`
		}
		if clause != "" {
			c += strings.Replace(clause, "connection_id", t+".connection_id", 1)
		}
		return c
	}
	args := []any{string(models.StatusCompleted)}
	if verified {
		args = append(args, string(models.VerificationOK))
	}
	args = append(args, connArgs...)
	query := `SELECT b.id, b.data FROM backups b JOIN (
			SELECT i.job_id, i.database_name, max(i.started_at) AS latest FROM backups i WHERE ` + cond("i") + ` GROUP BY i.job_id, i.database_name
		) l ON b.job_id = l.job_id AND b.database_name = l.database_name AND b.started_at = l.latest
		WHERE ` + cond("b") + ` ORDER BY b.started_at DESC, b.id DESC`
	list, err := listRecords[models.BackupRecord](ctx, s, tableBackups, nil, query, append(args, args...)...)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]*models.BackupRecord{}
	for _, b := range list {
		if out[b.JobID] == nil {
			out[b.JobID] = map[string]*models.BackupRecord{}
		}
		if _, seen := out[b.JobID][b.Database]; !seen {
			out[b.JobID][b.Database] = b
		}
	}
	return out, nil
}

// LatestRestoreTestsAll returns, per job, the newest restore test of each of its
// databases and the newest passed one (when that is older), newest first, in one
// query.
func (s *SQLiteStore) LatestRestoreTestsAll(ctx context.Context) (map[string][]*models.RestoreTestResult, error) {
	const db = `coalesce(json_extract(data, '$.database'), '')`
	query := `SELECT id, data FROM (
			SELECT id, data, job_id, started_at, status,
				ROW_NUMBER() OVER (PARTITION BY job_id, ` + db + ` ORDER BY started_at DESC, id DESC) AS rn,
				ROW_NUMBER() OVER (PARTITION BY job_id, ` + db + `, status = ? ORDER BY started_at DESC, id DESC) AS rn_status
			FROM restore_tests
		) WHERE rn = 1 OR (status = ? AND rn_status = 1) ORDER BY job_id, started_at DESC, id DESC`
	ok := string(models.RestoreTestOK)
	list, err := listRecords[models.RestoreTestResult](ctx, s, tableRestoreTests, nil, query, ok, ok)
	if err != nil {
		return nil, err
	}
	out := map[string][]*models.RestoreTestResult{}
	for _, t := range list {
		out[t.JobID] = append(out[t.JobID], t)
	}
	return out, nil
}

// LatestDRDrillsAll returns, per job, the newest disaster recovery drill (a
// restore test that read a copy target, see models.RestoreTestResult.SourceTargetID)
// of each of its databases and the newest passed one (when that is older), newest
// first, in one query.
func (s *SQLiteStore) LatestDRDrillsAll(ctx context.Context) (map[string][]*models.RestoreTestResult, error) {
	const db = `coalesce(json_extract(data, '$.database'), '')`
	query := `SELECT id, data FROM (
			SELECT id, data, job_id, started_at, status,
				ROW_NUMBER() OVER (PARTITION BY job_id, ` + db + ` ORDER BY started_at DESC, id DESC) AS rn,
				ROW_NUMBER() OVER (PARTITION BY job_id, ` + db + `, status = ? ORDER BY started_at DESC, id DESC) AS rn_status
			FROM restore_tests
			WHERE coalesce(json_extract(data, '$.source_target_id'), '') <> ''
		) WHERE rn = 1 OR (status = ? AND rn_status = 1) ORDER BY job_id, started_at DESC, id DESC`
	ok := string(models.RestoreTestOK)
	list, err := listRecords[models.RestoreTestResult](ctx, s, tableRestoreTests, nil, query, ok, ok)
	if err != nil {
		return nil, err
	}
	out := map[string][]*models.RestoreTestResult{}
	for _, t := range list {
		out[t.JobID] = append(out[t.JobID], t)
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
