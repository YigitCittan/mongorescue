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

// TestHistoryQueryPlans checks, on 20,000 backups, that every history query reads
// backups through an index (no "SCAN backups" or "SCAN b") and that the whole
// history stays fast.
func TestHistoryQueryPlans(t *testing.T) {
	s, now := historyFixture(t, 20, 20000)
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
	t.Logf("BackupHistory over 20,000 backups took %v", elapsed)
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
