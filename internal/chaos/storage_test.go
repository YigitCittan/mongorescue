//go:build chaos

package chaos

import (
	"strings"
	"testing"
	"time"
)

// TestStorageOutageMidUpload cuts the storage off in the middle of a multipart
// upload for ten seconds (every connection to MinIO is closed and new ones are
// refused), longer than the S3 client's own retries.
//
// Expected: the backup ends failed with the reason, carries no checksum or size, no
// object exists under its key, the multipart upload is aborted once the storage is
// back (within the 30 second abort window), backup.failed fires and the next backup
// completes and restores.
func TestStorageOutageMidUpload(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "outage")
	e.seedBlobs(t, db, "blobs", 48)

	// About 2 MiB/s towards the storage, so the outage lands mid-upload.
	e.Toxi.toxic(t, proxyMinio, "slow", "bandwidth", "upstream", map[string]any{"rate": 2048})
	b := r.startBackup(db)
	r.waitBytes(b.ID, 12<<20)
	key := r.objectKey(r.backup(db, b.ID).StorageKey)
	if r.incompleteUploads(key) == 0 {
		t.Fatal("no multipart upload in progress when the outage starts")
	}
	e.Toxi.setEnabled(t, proxyMinio, false)
	time.Sleep(10 * time.Second)
	e.Toxi.setEnabled(t, proxyMinio, true)

	got := r.waitBackup(db, b.ID)
	r.assertBrokenBackup(got, true)
	r.waitNoIncompleteUploads(key, 45*time.Second)
	r.assertNextBackupSucceeds(db, "blobs")
}

// TestStoragePartitionLongerThanTimeout partitions the storage for longer than
// storage_stall_timeout (1m, its minimum): connections stay open but no byte gets
// through in either direction, so writes block once the socket buffers are full,
// as when packets are dropped.
//
// Expected: the backup does not hang: it ends failed with the storage stall after
// that timeout (plus the bounded abort of its upload), carries no checksum or size, no object exists
// under its key, backup.failed fires, and the next backup after the partition
// completes. The parts of an upload that could not be aborted during the partition
// are not an object; they stay until a lifecycle rule removes them (see
// docs/production.md).
func TestStoragePartitionLongerThanTimeout(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	r.settings(map[string]any{"general": map[string]any{"storage_stall_timeout": "1m", "backup_timeout": "10m"}})
	db := e.uniqueDB(t, "partit")
	e.seedBlobs(t, db, "blobs", 48)

	e.Toxi.toxic(t, proxyMinio, "slow", "bandwidth", "upstream", map[string]any{"rate": 2048})
	b := r.startBackup(db)
	r.waitBytes(b.ID, 12<<20)
	start := time.Now()
	// An hour of latency holds every byte back and, unlike Toxiproxy's timeout
	// toxic (which keeps reading and buffering), pushes back on the sender.
	e.Toxi.toxic(t, proxyMinio, "partition-up", "latency", "upstream", map[string]any{"latency": 3600000})
	e.Toxi.toxic(t, proxyMinio, "partition-down", "latency", "downstream", map[string]any{"latency": 3600000})

	got := r.waitBackup(db, b.ID)
	took := time.Since(start)
	t.Logf("failed after %s: %s", took.Round(time.Second), got.ErrorMessage)
	r.assertBrokenBackup(got, true)
	if !strings.Contains(got.ErrorMessage, "no upload progress") {
		t.Fatalf("the backup failed for another reason than the storage stall: %s", got.ErrorMessage)
	}
	// Socket buffers draining, the stall timeout (1 min), the abort window (30 s)
	// and slack.
	if took > 3*time.Minute {
		t.Fatalf("the backup took %s to fail during the partition", took)
	}
	r.assertNextBackupSucceeds(db, "blobs")
}
