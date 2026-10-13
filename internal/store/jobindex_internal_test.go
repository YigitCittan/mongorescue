package store

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestJobColumnsBackfilledOnOpen checks that opening the database fills the
// filter columns of the jobs an older version wrote (index_version below
// jobIndexVersion), leaves an unreadable job alone, and changes nothing on the
// next open.
func TestJobColumnsBackfilledOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	s, err := OpenSQLite(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []*models.Job{
		{ID: "job_1", Name: "Shop", Database: "shop", CronExpression: "0 3 * * 1"},
		{ID: "job_2", Name: "Tenants", CronExpression: "@hourly", KnownDatabases: []string{"t_a"},
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"t_*"}}},
	} {
		if err = s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE jobs SET search_text = '', schedule = '', databases = '', discovers = 0, index_version = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO jobs (id, name, database_name, enabled, created_at, data) VALUES ('job_bad', 'bad', 'x', 0, 0, '{"id":"job_bad","name":42}')`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	if s, err = OpenSQLite(ctx, path, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	assertJobColumnsCurrent(t, s)
	var version int
	if err = s.db.QueryRowContext(ctx, "SELECT index_version FROM jobs WHERE id = 'job_bad'").Scan(&version); err != nil || version != 0 {
		t.Fatalf("the unreadable job: version %d, %v; want it left alone", version, err)
	}
	if n, cerr := s.CountJobs(ctx, JobQuery{Schedule: models.FrequencyWeekly}); cerr != nil || n != 1 {
		t.Fatalf("weekly jobs after the backfill = %d, %v", n, cerr)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLastStatusBackfilledByMigration builds a database at schema version 0028
// with jobs, backups and runs, and checks that migration 0029 fills every job's
// last status: a single-database job from its newest backup, a multi-database job
// from its newest run that was not skipped, a job without either as never.
func TestLastStatusBackfilledByMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 28 {
			break
		}
		if _, err = db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, execErr := db.ExecContext(ctx, q, args...); execErr != nil {
			t.Fatalf("%s: %v", q, execErr)
		}
	}
	for _, j := range []struct{ id, data string }{
		{"job_single", `{"id":"job_single","name":"s","database":"shop"}`},
		{"job_multi", `{"id":"job_multi","name":"m","database_selection":{"mode":"list","databases":["a","b"]}}`},
		{"job_never", `{"id":"job_never","name":"n","database":"shop"}`},
	} {
		exec(`INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, data) VALUES (?, ?, '', 1, 0, 'c1', ?)`, j.id, j.id, j.data)
	}
	for _, b := range []struct {
		id, job, status string
		at              int64
	}{
		{"b1", "job_single", "completed", 100}, {"b2", "job_single", "failed", 200}, {"b3", "job_multi", "failed", 300},
	} {
		exec(`INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, data) VALUES (?, ?, 'shop', ?, ?, 'c9', ?)`,
			b.id, b.job, b.status, b.at, `{"id":"`+b.id+`"}`)
	}
	for _, r := range []struct {
		id, status string
		at         int64
	}{{"r1", "partial", 150}, {"r2", "skipped", 400}} {
		exec(`INSERT INTO job_runs (id, job_id, started_at, status, data) VALUES (?, 'job_multi', ?, ?, ?)`, r.id, r.at, r.status, `{"id":"`+r.id+`"}`)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for id, want := range map[string]struct {
		rank, at sql.NullInt64
		conn     sql.NullString
	}{
		"job_single": {sql.NullInt64{Int64: 0, Valid: true}, sql.NullInt64{Int64: 200, Valid: true}, sql.NullString{String: "c9", Valid: true}},
		"job_multi":  {sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{Int64: 150, Valid: true}, sql.NullString{}},
		"job_never":  {},
	} {
		var rank, at sql.NullInt64
		var conn sql.NullString
		if err := s.db.QueryRowContext(ctx, "SELECT last_status, last_status_at, last_backup_connection FROM jobs WHERE id = ?", id).Scan(&rank, &at, &conn); err != nil {
			t.Fatal(err)
		}
		if rank != want.rank || at != want.at || conn != want.conn {
			t.Errorf("%s: last status %v at %v from %v; want %v at %v from %v", id, rank, at, conn, want.rank, want.at, want.conn)
		}
	}
}
