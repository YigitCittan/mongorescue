//go:build chaos

package chaos

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"

	// The metadata database driver, for integrity checks of a killed process's file.
	_ "modernc.org/sqlite"
)

// integrityCheck runs PRAGMA integrity_check on the metadata database of dataDir
// (the process must not be running) and fails unless SQLite reports "ok".
func integrityCheck(t *testing.T, dataDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "mongorescue.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if res != "ok" {
		t.Fatalf("the metadata database is corrupted: %s", res)
	}
}

// mongodumpRunning reports whether a mongodump of db is still running.
func mongodumpRunning(db string) bool {
	out, err := exec.Command("pgrep", "-f", "mongodump.*"+db).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// TestKillDuringBackup sends SIGKILL to the server in the middle of an upload and
// starts it again on the same data directory.
//
// Expected: the record is failed as interrupted (no checksum, no size), no object
// exists under its key, mongodump does not outlive the server, backup.failed fires
// after the restart, the metadata database passes an integrity check and the next
// backup completes. The parts of the killed multipart upload cannot be aborted by a
// dead process; they are not an object (see docs/production.md for the lifecycle
// rule that removes them).
func TestKillDuringBackup(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "killbkp")
	e.seedBlobs(t, db, "blobs", 32)

	e.Toxi.toxic(t, proxyMinio, "slow", "bandwidth", "upstream", map[string]any{"rate": 2048})
	b := r.startBackup(db)
	r.waitBytes(b.ID, 8<<20)
	key := r.objectKey(r.backup(db, b.ID).StorageKey)
	r.proc.kill()
	e.Toxi.reset(t)
	waitFor(t, 30*time.Second, "mongodump to exit after the server died", func() bool { return !mongodumpRunning(db) })
	integrityCheck(t, r.dataDir)

	r.restart()
	got := r.backup(db, b.ID)
	if !strings.Contains(got.ErrorMessage, "interrupted") {
		t.Fatalf("killed backup = %+v; want it failed as interrupted", got)
	}
	r.assertBrokenBackup(got, true)
	t.Logf("incomplete multipart uploads of the killed backup: %d", r.incompleteUploads(key))
	r.assertNextBackupSucceeds(db, "blobs")
}

// Environment of the purge helper process (TestPurgeHelper).
const (
	envPurgeHelperDir    = "MONGORESCUE_CHAOS_PURGE_DIR"
	envPurgeHelperPrefix = "MONGORESCUE_CHAOS_PURGE_PREFIX"
)

// TestPurgeHelper is not a test: TestKillDuringPurge runs the test binary with
// this test selected to purge the deleted backups of a stopped server's data
// directory, exactly as the server's maintenance does every ten minutes (with the
// clock moved past the grace period), so the purge can be killed mid-way without
// waiting for the schedule.
func TestPurgeHelper(t *testing.T) {
	dir := os.Getenv(envPurgeHelperDir)
	if dir == "" {
		t.Skip("helper process for TestKillDuringPurge")
	}
	ctx := context.Background()
	lock, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(ctx, filepath.Join(dir, "mongorescue.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s3st, err := storage.NewS3Storage(ctx, storage.S3Config{
		Endpoint: os.Getenv(envS3Proxy), Region: "us-east-1", Bucket: os.Getenv(envS3Bucket),
		Prefix: os.Getenv(envPurgeHelperPrefix), AccessKey: os.Getenv(envS3AccessKey),
		SecretKey: os.Getenv(envS3SecretKey), UsePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	purged, err := scheduler.PurgeDeleted(ctx, time.Now().Add(365*24*time.Hour), 7*24*time.Hour, st,
		func(context.Context, string) (storage.Storage, error) { return s3st, nil }, logger, nil)
	fmt.Printf("purged %d: %v\n", len(purged), err)
	if err != nil {
		t.Fatal(err)
	}
}

// runPurgeHelper runs TestPurgeHelper on dataDir; with killAfter > 0 it sends
// SIGKILL once that much time has passed.
func runPurgeHelper(t *testing.T, dataDir, prefix string, killAfter time.Duration) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPurgeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), envPurgeHelperDir+"="+dataDir, envPurgeHelperPrefix+"="+prefix)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if killAfter > 0 {
		select {
		case err := <-done:
			t.Fatalf("the purge finished before it was killed (%v): %s", err, out.String())
		case <-time.After(killAfter):
			_ = cmd.Process.Kill()
			<-done
			t.Logf("purge killed after %s", killAfter)
			return
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("purge helper: %v\n%s", err, out.String())
	}
}

// TestKillDuringPurge deletes ten backups (soft deletes, as retention does), then
// kills the purge that removes their archives in the middle (each S3 request is
// slowed down to about a second) and starts the server again.
//
// Expected: the metadata database passes an integrity check; no record that holds
// an archive (completed, deleted) has lost it except deleted ones whose purge was
// cut between the object delete and the record update, which the next purge
// finishes; purged records have no object; the kept backup still restores; a
// second purge completes and leaves exactly the kept backup's object.
func TestKillDuringPurge(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "purge")
	e.seedBlobs(t, db, "blobs", 1)

	var ids []string
	for range 11 {
		b := r.waitBackup(db, r.startBackup(db).ID)
		if b.Status != models.StatusCompleted {
			t.Fatalf("backup = %+v", b)
		}
		ids = append(ids, b.ID)
	}
	keep := ids[len(ids)-1]
	for _, id := range ids[:len(ids)-1] {
		r.api.data("DELETE", "/api/v1/backups/"+id, nil, http.StatusOK, nil)
	}
	r.proc.stop()

	e.Toxi.toxic(t, proxyMinio, "latency", "latency", "downstream", map[string]any{"latency": 1000})
	runPurgeHelper(t, r.dataDir, r.prefix, 4*time.Second)
	e.Toxi.reset(t)
	integrityCheck(t, r.dataDir)

	r.restart()
	exists := func(storageKey string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_, err := e.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(e.Bucket), Key: aws.String(r.objectKey(storageKey))})
		return err == nil
	}
	var list []*models.BackupRecord
	r.api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &list)
	purged, cut := 0, 0
	for _, b := range list {
		switch {
		case b.ID == keep:
			if b.Status != models.StatusCompleted || !exists(b.StorageKey) {
				t.Fatalf("the kept backup changed: %+v (object: %v)", b, exists(b.StorageKey))
			}
		case b.Status == models.StatusPurged:
			purged++
			if exists(b.StorageKey) {
				t.Fatalf("purged backup %s still has its object", b.ID)
			}
		case b.Status == models.StatusDeleted:
			if !exists(b.StorageKey) {
				cut++
			}
		default:
			t.Fatalf("backup %s is %s after the killed purge", b.ID, b.Status)
		}
	}
	t.Logf("after the kill: %d purged, %d deleted with the object already gone", purged, cut)
	if purged == len(ids)-1 {
		t.Fatal("the purge was not interrupted: every backup is purged")
	}
	if cut > 1 {
		t.Fatalf("%d deleted backups lost their object; a purge runs one backup at a time", cut)
	}

	r.proc.stop()
	runPurgeHelper(t, r.dataDir, r.prefix, 0)
	r.restart()
	r.api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &list)
	for _, b := range list {
		if b.ID != keep && (b.Status != models.StatusPurged || exists(b.StorageKey)) {
			t.Fatalf("after the second purge: %+v (object: %v)", b, exists(b.StorageKey))
		}
	}
	var accepted models.RestoreRecord
	r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": keep}, http.StatusAccepted, &accepted)
	if rst := r.waitRestore(accepted.ID); rst.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore of the kept backup = %+v", rst)
	}
}
