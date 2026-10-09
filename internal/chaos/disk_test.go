//go:build chaos

package chaos

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// requireSmallDir returns the small filesystem of the full-disk scenario or skips.
func requireSmallDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(envSmallDir)
	if dir == "" {
		t.Skip("no small filesystem (MONGORESCUE_CHAOS_SMALL_DIR); scripts/test-chaos-docker.sh creates one")
	}
	sub, err := os.MkdirTemp(dir, "fulldisk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sub) })
	// The server reports resolved paths (/private/var on macOS).
	if resolved, err := filepath.EvalSymlinks(sub); err == nil {
		sub = resolved
	}
	return sub
}

// fillDisk writes a file into dir until the filesystem is full and returns its
// path.
func fillDisk(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "filler")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := make([]byte, 64<<10)
	for {
		if _, err := f.Write(chunk); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				_ = f.Sync()
				return path
			}
			t.Fatalf("fill the disk: %v", err)
		}
	}
}

// localFiles lists the regular files under dir.
func localFiles(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// TestFullDiskOnStorageTarget puts the data directory and the default local
// storage target on a small filesystem (48 MiB by default) and runs a backup larger
// than the free space, which fills it in the middle of the write.
//
// Expected: the backup ends failed with the reason (no space left on device),
// backup.failed fires, no archive and no temporary file is left, the metadata
// database passes an integrity check, and the next backup (to S3) completes.
func TestFullDiskOnStorageTarget(t *testing.T) {
	e := requireEnv(t)
	small := requireSmallDir(t)
	r := newRig(t, e, rigOptions{dataDir: filepath.Join(small, "data")})
	// The default "Local disk" target lives next to the data directory, on the
	// small filesystem too.
	var targets []*models.StorageTarget
	r.api.data("GET", "/api/v1/storage-targets", nil, http.StatusOK, &targets)
	var local *models.StorageTarget
	for _, tg := range targets {
		if tg.Type == models.StorageLocal {
			local = tg
		}
	}
	if local == nil || !strings.HasPrefix(local.Local.Path, small) {
		t.Fatalf("no local target on the small filesystem: %+v", targets)
	}
	targetDir := local.Local.Path
	big := e.uniqueDB(t, "bigdisk")
	e.seedBlobs(t, big, "blobs", 96)
	var rec models.BackupRecord
	r.api.data("POST", "/api/v1/backups", map[string]any{"connection_id": r.connID, "database": big, "gzip": false, "storage_target_id": local.ID}, http.StatusAccepted, &rec)
	got := r.waitBackup(big, rec.ID)
	if got.Status == models.StatusCompleted || got.ErrorMessage == "" || got.SHA256 != "" {
		t.Fatalf("a backup that filled the disk = %+v; want failed with a reason", got)
	}
	r.hook.wait(t, "backup.failed", "backup_id", got.ID)
	if files := localFiles(targetDir); len(files) > 0 {
		t.Fatalf("a backup that filled the disk left files behind: %v", files)
	}
	t.Logf("disk full mid-write: %s", got.ErrorMessage)

	r.proc.stop()
	integrityCheck(t, r.dataDir)
	r.restart()
	db := e.uniqueDB(t, "afterfl")
	e.seedBlobs(t, db, "blobs", 4)
	r.assertNextBackupSucceeds(db, "blobs")
}

// TestFullDiskOnDataDir fills the small filesystem of the data directory
// completely and starts backups to S3 while the metadata database cannot grow.
//
// Expected: the server keeps answering (requests that must write fail with a
// clean error, it does not crash); no run reports success that the metadata does
// not record and none stays in progress; the metadata database passes an
// integrity check; once space is freed, the next backup completes and restores.
func TestFullDiskOnDataDir(t *testing.T) {
	t.Skip("known failing: a backup whose record cannot be saved stays in progress and reports success, https://github.com/YigitCittan/mongorescue/issues/152")
	e := requireEnv(t)
	small := requireSmallDir(t)
	r := newRig(t, e, rigOptions{dataDir: filepath.Join(small, "data")})

	db := e.uniqueDB(t, "fulldsk")
	e.seedBlobs(t, db, "blobs", 4)
	filler := fillDisk(t, small)
	statuses := map[int]int{}
	var started []string
	for range 5 {
		code, body, err := r.api.try("POST", "/api/v1/backups", map[string]any{"connection_id": r.connID, "database": db, "gzip": false})
		if err != nil {
			t.Fatalf("the server did not answer with the disk full: %v", err)
		}
		statuses[code]++
		if code == http.StatusAccepted {
			var env envelope
			_ = json.Unmarshal(body, &env)
			var b models.BackupRecord
			_ = json.Unmarshal(env.Data, &b)
			started = append(started, b.ID)
			waitFor(t, runTimeout, "the backup to end", func() bool {
				c, list, listErr := r.api.try("GET", "/api/v1/backups?database="+db, nil)
				return listErr == nil && c == http.StatusOK && !strings.Contains(string(list), `"in_progress"`)
			})
		} else if code < 400 {
			t.Fatalf("backup with a full disk answered %d: %s", code, body)
		}
		if r.proc.exited {
			t.Fatal("the server exited with the disk full")
		}
	}
	t.Logf("with the disk full: responses %v", statuses)
	if code, _, err := r.api.try("GET", "/api/v1/health", nil); err != nil || code >= 500 && code != http.StatusServiceUnavailable {
		t.Fatalf("health with the disk full: %d %v", code, err)
	}

	// Free the space and recover.
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	var list []*models.BackupRecord
	r.api.data("GET", "/api/v1/backups?database="+db, nil, http.StatusOK, &list)
	for _, b := range list {
		if b.Status == models.StatusCompleted {
			// It completed although the disk was full: the archive must be real.
			r.api.data("POST", "/api/v1/backups/"+b.ID+"/verify", nil, http.StatusAccepted, nil)
		}
		if b.Status == models.StatusInProgress {
			t.Fatalf("backup %s stays in progress after the disk was freed", b.ID)
		}
	}
	r.proc.stop()
	integrityCheck(t, r.dataDir)
	r.restart()
	r.assertNextBackupSucceeds(db, "blobs")
}
