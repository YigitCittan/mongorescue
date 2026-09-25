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

// TestBackupsWrittenBeforeTriggersAreBackfilled applies the migrations up to 0007,
// stores backups the way earlier releases did (without a trigger) and checks that
// opening the database with this build backfills job backups as scheduled and the
// others as manual, leaving recorded triggers alone.
func TestBackupsWrittenBeforeTriggersAreBackfilled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 7 {
			break
		}
		if _, err = db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?, ?, ?)", m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ id, job, data string }{
		{"bkp_job", "job_1", `{"id":"bkp_job","job_id":"job_1","database":"shop","status":"completed"}`},
		{"bkp_manual", "", `{"id":"bkp_manual","database":"shop","status":"completed"}`},
		{"bkp_mcp", "", `{"id":"bkp_mcp","database":"shop","status":"completed","trigger":"mcp"}`},
	} {
		if _, err = db.ExecContext(ctx, `INSERT INTO backups (id, job_id, database_name, status, started_at, data) VALUES (?, ?, 'shop', 'completed', 1, ?)`,
			row.id, row.job, row.data); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(ctx, path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for id, want := range map[string]models.BackupTrigger{
		"bkp_job": models.TriggerScheduled, "bkp_manual": models.TriggerManual, "bkp_mcp": models.TriggerMCP,
	} {
		rec, err := s.GetBackupRecord(ctx, id)
		if err != nil || rec.Trigger != want {
			t.Errorf("%s = %+v, %v; want trigger %q", id, rec, err, want)
		}
	}
}
