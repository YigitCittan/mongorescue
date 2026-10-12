//go:build chaos

package chaos

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestMongoDropDuringDump closes every connection to MongoDB in the middle of a
// dump and refuses new ones for five seconds.
//
// Expected: the backup ends failed with the reason (no password in it), no checksum
// or size, no object under its key and no incomplete multipart upload, backup.failed
// fires, and the next backup completes and restores.
func TestMongoDropDuringDump(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "dropdmp")
	e.seedBlobs(t, db, "blobs", 32)

	e.Toxi.toxic(t, proxyMongo, "slow", "bandwidth", "downstream", map[string]any{"rate": 4096})
	b := r.startBackup(db)
	r.waitBytes(b.ID, 8<<20)
	key := r.objectKey(r.backup(db, b.ID).StorageKey)
	e.Toxi.setEnabled(t, proxyMongo, false)
	time.Sleep(5 * time.Second)
	e.Toxi.setEnabled(t, proxyMongo, true)

	got := r.waitBackup(db, b.ID)
	r.assertBrokenBackup(got, true)
	r.waitNoIncompleteUploads(key, 45*time.Second)
	r.assertNextBackupSucceeds(db, "blobs")
}

// TestMongoDropDuringRestore cuts MongoDB off in the middle of restores into safe
// clones (every connection closed, new ones refused). mongorestore rides out an
// outage while its server selection waits, so the outcome of a 5 s outage may be
// either; then, with restore_timeout at 30 s, a 45 s outage.
//
// Expected: a restore never ends completed with missing documents: either it
// completes with every document, or it ends failed with the reason (no password in
// it) and restore.failed. With the 45 s outage it fails on the timeout, the source
// is untouched and the partial clone is dropped or named by the failed record
// (marked, so it can be dropped). The next restore completes with every document.
func TestMongoDropDuringRestore(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "droprst")
	e.seedBlobs(t, db, "blobs", 32)
	want := e.count(t, db, "blobs")

	b := r.waitBackup(db, r.startBackup(db).ID)
	if b.Status != models.StatusCompleted {
		t.Fatalf("baseline backup = %+v", b)
	}

	e.Toxi.toxic(t, proxyMongo, "slow", "bandwidth", "upstream", map[string]any{"rate": 4096})
	var accepted models.RestoreRecord
	r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": b.ID}, http.StatusAccepted, &accepted)
	r.waitBytes(accepted.ID, 12<<20)
	e.Toxi.setEnabled(t, proxyMongo, false)
	time.Sleep(5 * time.Second)
	e.Toxi.setEnabled(t, proxyMongo, true)
	blip := r.waitRestore(accepted.ID)
	if blip.Status == models.RestoreStatusCompleted {
		if n := e.count(t, blip.TargetDatabase, "blobs"); n != want {
			t.Fatalf("a restore across a 5 s outage completed with %d of %d documents", n, want)
		}
		t.Log("a 5 s outage was ridden out: the restore completed with every document")
	} else {
		t.Logf("a 5 s outage failed the restore: %s", blip.ErrorMessage)
	}

	r.settings(map[string]any{"general": map[string]any{"restore_timeout": "30s"}})
	r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": b.ID}, http.StatusAccepted, &accepted)
	r.waitBytes(accepted.ID, 12<<20)
	e.Toxi.setEnabled(t, proxyMongo, false)
	time.Sleep(45 * time.Second)
	e.Toxi.setEnabled(t, proxyMongo, true)

	rst := r.waitRestore(accepted.ID)
	if rst.Status == models.RestoreStatusCompleted {
		n := e.count(t, rst.TargetDatabase, "blobs")
		t.Fatalf("a restore across a 45 s outage with a 30 s timeout ended completed (%d of %d documents): %+v", n, want, rst)
	}
	if rst.Status != models.RestoreStatusFailed || rst.ErrorMessage == "" {
		t.Fatalf("broken restore = %+v; want failed with a reason", rst)
	}
	t.Logf("the restore failed: %s", rst.ErrorMessage)
	assertNoSecret(t, e.MongoPassword, "restore error", rst.ErrorMessage)
	r.hook.wait(t, "restore.failed", "restore_id", rst.ID)
	if got := e.count(t, db, "blobs"); got != want {
		t.Fatalf("the source changed: %d documents, want %d", got, want)
	}
	if !strings.HasPrefix(rst.TargetDatabase, db+"_rescue_") {
		t.Fatalf("restore target = %q", rst.TargetDatabase)
	}
	if e.databaseExists(t, rst.TargetDatabase) {
		t.Logf("partial clone %s kept, named by the failed restore %s", rst.TargetDatabase, rst.ID)
	}

	r.env.Toxi.reset(t)
	r.settings(map[string]any{"general": map[string]any{"restore_timeout": "12h"}})
	r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": b.ID}, http.StatusAccepted, &accepted)
	next := r.waitRestore(accepted.ID)
	if next.Status != models.RestoreStatusCompleted {
		t.Fatalf("the next restore did not complete: %+v", next)
	}
	if got := e.count(t, next.TargetDatabase, "blobs"); got != want {
		t.Fatalf("the next restore holds %d documents, want %d", got, want)
	}
	r.hook.wait(t, "restore.succeeded", "restore_id", next.ID)
}

// stepDown makes the primary of the single-node replica set step down for secs
// seconds and waits until it is primary again.
func (e *env) stepDown(t *testing.T, secs int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	// The command closes connections, so its error is expected.
	err := e.Mongo.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetStepDown", Value: secs}, {Key: "force", Value: true}}).Err()
	t.Logf("replSetStepDown: %v", err)
}

// waitPrimary waits until the member is a writable primary again.
func (e *env) waitPrimary(t *testing.T) {
	t.Helper()
	waitFor(t, 2*time.Minute, "a primary", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var hello struct {
			IsWritablePrimary bool `bson:"isWritablePrimary"`
		}
		err := e.Mongo.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
		return err == nil && hello.IsWritablePrimary
	})
}

// TestPrimaryStepdownDuringDump steps the primary down in the middle of a dump. CI
// and local runs use a single-node replica set, so the member steps down, has no
// primary for ten seconds and is elected again: the failover a dump sees, without
// a second member to continue on.
//
// Expected: the backup either fails (failed, reason, no checksum, no object,
// backup.failed) or completes with an archive that restores to exactly the source;
// it never completes with partial data. The next backup completes.
func TestPrimaryStepdownDuringDump(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	db := e.uniqueDB(t, "stepdmp")
	e.seedBlobs(t, db, "blobs", 32)
	want := e.count(t, db, "blobs")

	e.Toxi.toxic(t, proxyMongo, "slow", "bandwidth", "downstream", map[string]any{"rate": 4096})
	b := r.startBackup(db)
	r.waitBytes(b.ID, 8<<20)
	e.stepDown(t, 10)
	got := r.waitBackup(db, b.ID)
	e.waitPrimary(t)
	e.Toxi.reset(t)

	switch got.Status {
	case models.StatusCompleted:
		var accepted models.RestoreRecord
		r.api.data("POST", "/api/v1/restore", map[string]any{"backup_id": got.ID}, http.StatusAccepted, &accepted)
		rst := r.waitRestore(accepted.ID)
		if rst.Status != models.RestoreStatusCompleted {
			t.Fatalf("restore of the backup taken across the stepdown = %+v", rst)
		}
		if n := e.count(t, rst.TargetDatabase, "blobs"); n != want {
			t.Fatalf("the backup taken across the stepdown completed with %d of %d documents", n, want)
		}
		t.Log("the dump survived the stepdown and restores completely")
	default:
		r.assertBrokenBackup(got, true)
		t.Logf("the dump failed across the stepdown: %s", got.ErrorMessage)
	}
	r.assertNextBackupSucceeds(db, "blobs")
}
