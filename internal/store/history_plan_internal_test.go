package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// historyFixture opens a store with jobs jobs and rows backups spread over 90 days
// (written in one transaction, straight into the table).
func historyFixture(tb testing.TB, jobs, rows int) (*SQLiteStore, time.Time) {
	tb.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(tb.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for j := range jobs {
		if err = s.SaveJob(ctx, &models.Job{ID: fmt.Sprintf("job_%02d", j), Name: "j", Database: "db", CronExpression: "@hourly", Enabled: true}); err != nil {
			tb.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO backups (id, job_id, database_name, status, started_at, size_bytes, data) VALUES (?, ?, 'db', ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatal(err)
	}
	statuses := []models.BackupStatus{models.StatusCompleted, models.StatusCompleted, models.StatusCompleted, models.StatusFailed}
	step := 90 * 24 * time.Hour / time.Duration(rows)
	for i := range rows {
		started := now.Add(-time.Duration(i) * step)
		status := statuses[i%len(statuses)]
		rec := map[string]any{"id": fmt.Sprintf("b%06d", i), "status": status, "duration_seconds": i % 300}
		if i%97 == 0 {
			rec["verification"] = models.VerificationMismatch
		}
		data, _ := json.Marshal(rec)
		if _, err := stmt.ExecContext(ctx, rec["id"], fmt.Sprintf("job_%02d", i%jobs), string(status), started.UnixNano(), 1000, string(data)); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ANALYZE"); err != nil {
		tb.Fatal(err)
	}
	return s, now
}

// scaleStats are the sqlite_stat1 rows ANALYZE records for the jobs and backups at
// the scale the plan tests check (TestScaleQueryPlans' 10,000 jobs on 500
// connections, 20 jobs with 20,000 backups). Record them again (run ANALYZE on that
// data and copy the rows) when a migration adds or changes an index of these tables.
var scaleStats = []struct{ tbl, idx, stat string }{
	{"backups", "backups_by_connection", "20000 20000 20000"},
	{"backups", "backups_by_database", "20000 20000 1 1"},
	{"backups", "backups_by_job_database", "20000 1000 1000 1000 1"},
	{"backups", "backups_by_job_started", "20000 1000 1 1"},
	{"backups", "backups_by_retry_of_started", "20000 20000 1 1"},
	{"backups", "backups_by_run", "20000 20000 1 1"},
	{"backups", "backups_by_started", "20000 1 1"},
	{"backups", "backups_by_started_status_size", "20000 1 1 1"},
	{"backups", "backups_by_status_started", "20000 10000 1 1"},
	{"backups", "backups_by_status_started_size", "20000 10000 1 1"},
	{"backups", "backups_by_storage_target", "20000 20000 10000"},
	{"backups", "sqlite_autoindex_backups_1", "20000 1"},
	{"jobs", "jobs_by_connection_created", "10020 20 20 1"},
	{"jobs", "jobs_by_connection_last_status", "10020 20 20 20 1"},
	{"jobs", "jobs_by_connection_name", "10020 20 1 1"},
	{"jobs", "jobs_by_connection_next_run", "5020 20 1 1"},
	{"jobs", "jobs_by_created", "10020 478 1"},
	{"jobs", "jobs_by_enabled_created", "10020 5010 456 1"},
	{"jobs", "jobs_by_enabled_last_status", "10020 5010 2505 456 1"},
	{"jobs", "jobs_by_enabled_name", "10020 5010 1 1"},
	{"jobs", "jobs_by_last_status", "10020 3340 478 1"},
	{"jobs", "jobs_by_name", "10020 1 1"},
	{"jobs", "jobs_by_next_run", "5020 60 1"},
	{"jobs", "jobs_by_storage_target", "10020 10020"},
	{"jobs", "jobs_discovering", "0 0"},
	{"jobs", "jobs_filter", "10020 5010 3340 2505 20 20 1 1 1 1 1"},
	{"jobs", "sqlite_autoindex_jobs_1", "10020 1"},
}

// useScaleStats makes the planner plan as at the scale of scaleStats: the plan
// tests seed a few hundred rows instead of tens of thousands (they run under -race
// in the unit suite), and the plans depend on the statistics, not the rows.
func useScaleStats(tb testing.TB, s *SQLiteStore) {
	tb.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sqlite_stat1 WHERE tbl IN ('jobs', 'backups')"); err != nil {
		tb.Fatal(err)
	}
	// The samples of the seeded rows (SQLITE_ENABLE_STAT4) would contradict them.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sqlite_stat4 WHERE tbl IN ('jobs', 'backups')"); err != nil {
		tb.Fatal(err)
	}
	for _, r := range scaleStats {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES (?, ?, ?)", r.tbl, r.idx, r.stat); err != nil {
			tb.Fatal(err)
		}
	}
	// Reloads the statistics into the connection's planner.
	if _, err := s.db.ExecContext(ctx, "ANALYZE sqlite_schema"); err != nil {
		tb.Fatal(err)
	}
}

func historyDayStarts(now time.Time, days int) []time.Time {
	first := time.Date(now.Year(), now.Month(), now.Day()-days+1, 0, 0, 0, 0, time.UTC)
	out := make([]time.Time, days+1)
	for i := range out {
		out[i] = first.AddDate(0, 0, i)
	}
	return out
}

// queryPlan returns the detail lines of EXPLAIN QUERY PLAN for query.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// TestHistoryQueryPlans checks, with the statistics of 20,000 backups (1,000
// seeded, see useScaleStats), that every history query reads backups through an
// index (no "SCAN backups" or "SCAN b").
func TestHistoryQueryPlans(t *testing.T) {
	s, now := historyFixture(t, 20, 1000)
	useScaleStats(t, s)
	starts := historyDayStarts(now, 30)
	from := timeKey(starts[0])
	completed := string(models.StatusCompleted)
	dayArgs := []any{}
	for i := range 30 {
		dayArgs = append(dayArgs, i, timeKey(starts[i]), timeKey(starts[i+1]))
	}
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"days":         {historyDaysSQL(30), dayArgs},
		"bytes before": {historyBytesBeforeSQL, []any{completed, from}},
		"job runs":     {historyRunsSQL, []any{12}},
		"last success": {historyLastSuccessSQL, []any{completed}},
		"verification": {historyVerificationSQL, []any{completed, from, "mismatch", "error", 20}},
	} {
		plan := queryPlan(t, s.db, q.sql, q.args...)
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN backups") || strings.HasPrefix(line, "SCAN b ") || line == "SCAN b" {
				t.Errorf("%s scans the backups table: %q (plan %q)", name, line, plan)
			}
		}
		if !strings.Contains(strings.Join(plan, "\n"), "USING") {
			t.Errorf("%s uses no index: %q", name, plan)
		}
		// Per job, the newest backups come from the (job_id, started_at) index.
		if (name == "job runs" || name == "last success") && !strings.Contains(strings.Join(plan, "\n"), "backups_by_job_started (job_id=?)") {
			t.Errorf("%s does not search the job index: %q", name, plan)
		}
	}

	start := time.Now()
	h, err := s.BackupHistory(context.Background(), BackupHistoryQuery{DayStarts: starts, RunsPerJob: 12, MaxIssues: 20})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// Jobs 3, 7, 11, 15 and 19 only have failed backups (every fourth row fails).
	if len(h.JobRuns) != 20 || len(h.JobRuns["job_00"]) != 12 || len(h.LastSuccess) != 15 || h.VerificationIssueTotal == 0 || len(h.Days) != 30 {
		t.Fatalf("history = %d jobs, %d runs, %d successes, %d issues, %d days", len(h.JobRuns), len(h.JobRuns["job_00"]),
			len(h.LastSuccess), h.VerificationIssueTotal, len(h.Days))
	}
	// The query plans above are what keeps this fast; wall-clock limits are not
	// asserted because CI runs this under -race and coverage on shared runners
	// (BenchmarkBackupHistory measures the speed).
	t.Logf("BackupHistory over 1,000 backups took %v", elapsed)
}

// BenchmarkBackupHistory measures the overview's query on 20,000 backups.
func BenchmarkBackupHistory(b *testing.B) {
	s, now := historyFixture(b, 20, 20000)
	q := BackupHistoryQuery{DayStarts: historyDayStarts(now, 30), RunsPerJob: 12, MaxIssues: 20}
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.BackupHistory(ctx, q); err != nil {
			b.Fatal(err)
		}
	}
}
