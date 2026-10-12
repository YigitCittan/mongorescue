//go:build chaos

package chaos

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// fixtureVersion is the schema version of the old metadata database the migration
// scenario upgrades: twelve schema migrations and the one-time sealing of
// plaintext credentials behind the current release.
const fixtureVersion = 12

// fixtureBackups is the number of backup rows of the fixture, so the migrations
// that rebuild indexes and backfill columns take long enough to be killed inside.
const fixtureBackups = 20000

// buildOldDatabase writes the metadata database of a release at fixtureVersion into
// dir: the repository's own migrations 0001..fixtureVersion applied one by one with
// their schema_migrations rows (as the store applies them), then a connection with
// a plaintext URI (as releases before sealed secrets stored it), a local storage
// target, a job and fixtureBackups completed backups. No user exists, so the
// upgraded server starts in setup mode. Nothing is downloaded.
func buildOldDatabase(t *testing.T, dir, mongoURI, backupsDir string) (connID string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v", err)
	}
	sort.Strings(files)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "mongorescue.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, execErr := db.Exec(q, args...); execErr != nil {
			t.Fatalf("fixture: %v\n%s", execErr, q)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		var version int
		if _, scanErr := fmt.Sscanf(name, "%04d_", &version); scanErr != nil {
			t.Fatalf("migration name %s: %v", name, scanErr)
		}
		if version > fixtureVersion {
			break
		}
		body, readErr := os.ReadFile(f)
		if readErr != nil {
			t.Fatal(readErr)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, version, name, time.Now().UTC().Format(time.RFC3339Nano))
	}

	now := time.Now().UTC()
	conn := models.Connection{ID: "conn_fixture", Name: "fixture", URI: mongoURI}
	raw, _ := json.Marshal(conn)
	exec(`INSERT INTO connections (id, name, data) VALUES (?, ?, ?)`, conn.ID, conn.Name, string(raw))
	target := models.StorageTarget{ID: "tgt_fixture", Name: "disk", Type: models.StorageLocal, IsDefault: true,
		Local: &models.LocalTarget{Path: backupsDir}}
	raw, _ = json.Marshal(target)
	exec(`INSERT INTO storage_targets (id, name, is_default, data) VALUES (?, ?, 1, ?)`, target.ID, target.Name, string(raw))
	job := models.Job{ID: "job_fixture", Name: "fixture", Database: "fixture", CronExpression: "@daily", Enabled: false,
		ConnectionID: conn.ID, StorageTargetID: target.ID, RetentionCount: 7, CreatedAt: now}
	raw, _ = json.Marshal(job)
	exec(`INSERT INTO jobs (id, name, database_name, enabled, created_at, data, connection_id, storage_target_id) VALUES (?, ?, ?, 0, ?, ?, ?, ?)`,
		job.ID, job.Name, job.Database, now.Unix(), string(raw), conn.ID, target.ID)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO backups (id, job_id, database_name, status, started_at, data, connection_id, storage_target_id, size_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fixtureBackups {
		started := now.Add(-time.Duration(fixtureBackups-i) * time.Hour)
		done := started.Add(time.Minute)
		rec := models.BackupRecord{
			ID: fmt.Sprintf("bkp_fixture_%06d", i), JobID: job.ID, Database: "fixture", ConnectionID: conn.ID,
			Status: models.StatusCompleted, StorageType: models.StorageLocal, StorageTargetID: target.ID,
			StorageKey: fmt.Sprintf("fixture/%06d.archive.gz", i), SizeBytes: 1024, StartedAt: started, CompletedAt: &done,
		}
		raw, _ := json.Marshal(rec)
		if _, err := stmt.Exec(rec.ID, rec.JobID, rec.Database, string(rec.Status), started.Unix(), string(raw), conn.ID, target.ID, rec.SizeBytes); err != nil {
			t.Fatalf("fixture backup: %v", err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return conn.ID
}

// copyDir copies the files of src into the empty directory dst.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

const migrationLine = "applied metadata schema migration"

// TestKillDuringMigration upgrades the metadata database of an older schema with
// the current binary and SIGKILLs it right after each schema migration it logs
// (from inside the write of the log line) and at points spread over the rest of its
// start-up (the one-time sealing of plaintext credentials and the rest), each time
// on a fresh copy of the old database. Then it starts the current binary again.
//
// Expected: the killed file passes an integrity check; the next start completes the
// upgrade with exactly the missing migrations (none half-applied: a migration
// commits together with its schema_migrations row); every old backup is still
// listed as completed; the connection's credentials were sealed and still connect;
// and a backup and a restore work on the upgraded installation.
func TestKillDuringMigration(t *testing.T) {
	e := requireEnv(t)
	golden := t.TempDir()
	backupsDir := filepath.Join(t.TempDir(), "backups")
	connID := buildOldDatabase(t, golden, e.MongoURI, backupsDir)

	// The migrations the current binary applies to the fixture.
	port := freePort(t)
	probeDir := t.TempDir()
	copyDir(t, golden, probeDir)
	startedAt := time.Now()
	p := startProc(t, binary(t), probeDir, port)
	window := time.Since(startedAt)
	applied := strings.Count(p.logs.String(), migrationLine)
	p.stop()
	if applied == 0 {
		t.Fatal("the current binary applied no migration to the fixture")
	}
	t.Logf("fixture at schema %d: %d migrations to apply; the upgraded server is ready after %s", fixtureVersion, applied, window)

	// killAt kills an upgrade of a fresh copy (after k logged migrations, or delay
	// after its start when k < 0), checks the recovery and returns how many
	// migrations the killed process applied.
	n := 0
	killAt := func(k int, delay time.Duration, last bool) int {
		i := n
		n++
		work := t.TempDir()
		copyDir(t, golden, work)
		var victim *proc
		if k >= 0 {
			victim = launchKilling(t, binary(t), work, port, func(out string) bool { return strings.Count(out, migrationLine) >= k })
			select {
			case <-victim.done:
				victim.exited = true
			case <-time.After(60 * time.Second):
				t.Fatalf("kill point %d: the upgrade did not log %d migrations", i, k)
			}
		} else {
			victim = launch(t, binary(t), work, port)
			time.Sleep(delay)
			victim.kill()
		}
		before := strings.Count(victim.logs.String(), migrationLine)
		integrityCheck(t, work)

		next := startProc(t, binary(t), work, port)
		after := strings.Count(next.logs.String(), migrationLine)
		if before+after != applied {
			t.Fatalf("kill point %d: %d migrations before the kill and %d after, want %d in all", i, before, after, applied)
		}
		a := newAPI(t, next.base)
		match := setupCodePattern.FindStringSubmatch(next.logs.String())
		if match == nil {
			t.Fatalf("kill point %d: no setup code after the upgrade", i)
		}
		var session struct {
			CSRFToken string `json:"csrf_token"`
		}
		a.data("POST", "/api/v1/setup", map[string]string{"setup_code": match[1], "username": adminUser, "password": adminPass}, http.StatusCreated, &session)
		a.csrf = session.CSRFToken
		code, body := a.do("GET", "/api/v1/backups?database=fixture&status=completed&limit=1", nil)
		var page struct {
			Meta struct {
				Total int `json:"total"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(body, &page); code != http.StatusOK || err != nil || page.Meta.Total != fixtureBackups {
			t.Fatalf("kill point %d: completed fixture backups after the upgrade: %d %s, want %d", i, code, body, fixtureBackups)
		}
		var probe struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		a.data("POST", "/api/v1/connections/"+connID+"/test", nil, http.StatusOK, &probe)
		if !probe.OK {
			t.Fatalf("kill point %d: the upgraded connection does not connect: %+v", i, probe)
		}
		if last {
			db := e.uniqueDB(t, "migr")
			e.seedBlobs(t, db, "blobs", 1)
			var rec models.BackupRecord
			a.data("POST", "/api/v1/backups", map[string]any{"connection_id": connID, "database": db}, http.StatusAccepted, &rec)
			r := &rig{t: t, env: e, api: a}
			if b := r.waitBackup(db, rec.ID); b.Status != models.StatusCompleted {
				t.Fatalf("backup after the upgrade = %+v", b)
			}
			var accepted models.RestoreRecord
			a.data("POST", "/api/v1/restore", map[string]any{"backup_id": rec.ID}, http.StatusAccepted, &accepted)
			if rst := r.waitRestore(accepted.ID); rst.Status != models.RestoreStatusCompleted {
				t.Fatalf("restore after the upgrade = %+v", rst)
			}
		}
		next.stop()
		// The plaintext URI of the old database must be gone from every file of the
		// data directory once the upgrade sealed it.
		for _, f := range localFiles(work) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if e.MongoPassword != "" && strings.Contains(string(raw), e.MongoPassword) {
				t.Fatalf("kill point %d: the connection password is still stored in plaintext in %s", i, filepath.Base(f))
			}
		}
		t.Logf("kill point %d (after %d logged migrations or %s): killed after %d migrations, the restart applied %d", i, k, delay, before, after)
		return before
	}

	// A kill right after each logged migration (from inside the write of the log
	// line, so it lands before or during the next one), then kills at times spread
	// over the rest of the start-up: the one-time sealing of secrets and the rest.
	inside := 0
	for k := 1; k < applied; k++ {
		if got := killAt(k, 0, false); got < applied {
			inside++
		}
	}
	const later = 6
	for i := range later {
		killAt(-1, window*time.Duration(i)/later, false)
	}
	killAt(-1, window, true)
	if inside == 0 {
		t.Fatal("no kill landed before the last schema migration")
	}
	t.Logf("%d of %d kills landed between two schema migrations", inside, n)
}
