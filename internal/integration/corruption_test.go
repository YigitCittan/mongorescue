//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// seedText inserts n documents with 4 KiB lowercase strings into db.coll, so most
// bytes of an uncompressed archive are string content that mongorestore accepts
// even when a byte is changed.
func (m *mongoEnv) seedText(t *testing.T, db, coll string, n int) {
	t.Helper()
	docs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "text", Value: strings.Repeat(string(rune('a'+i%26)), 4096)}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := m.Client.Database(db).Collection(coll).InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed %s.%s: %v", db, coll, err)
	}
}

// flipLetterAt returns a transform that changes the first lowercase letter at or
// after offset at into another letter: the archive stays well-formed.
func flipLetterAt(at int64) func(off int64, p []byte) int {
	done := false
	return func(off int64, p []byte) int {
		for i := range p {
			if done || off+int64(i) < at {
				continue
			}
			if c := p[i]; c >= 'a' && c <= 'z' {
				if c == 'z' {
					p[i] = 'y'
				} else {
					p[i] = c + 1
				}
				done = true
			}
		}
		return len(p)
	}
}

// markerIndex names an index whose name is found in the archive's metadata.
const markerIndex = "marker_index_by_text"

// archiveOffset returns the offset of marker in the stored object at key.
func archiveOffset(t *testing.T, st storage.Storage, key, marker string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	rc, err := st.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("retrieve %s: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc) // test archives are small
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	i := bytes.Index(data, []byte(marker))
	if i < 0 {
		t.Fatalf("%q not found in the archive", marker)
	}
	return int64(i)
}

// flipByteAt returns a transform that inverts the byte at offset at.
func flipByteAt(at int64) func(off int64, p []byte) int {
	return func(off int64, p []byte) int {
		if at >= off && at < off+int64(len(p)) {
			p[at-off] ^= 0xff
		}
		return len(p)
	}
}

// truncateAt returns a transform that ends the stream after n bytes.
func truncateAt(n int64) func(off int64, p []byte) int {
	return func(off int64, p []byte) int {
		return int(max(min(int64(len(p)), n-off), 0))
	}
}

// damaged stores a copy of src's artifact altered by transform and returns a record
// pointing at it (with the checksum recorded for the intact artifact).
func damaged(t *testing.T, st storage.Storage, src *models.BackupRecord, transform func(off int64, p []byte) int) *models.BackupRecord {
	t.Helper()
	rec := *src
	rec.StorageKey = "damaged/" + randomHex(t, 4) + "/" + src.StorageKey
	copyObject(t, st, src.StorageKey, rec.StorageKey, transform)
	if objectSHA256(t, st, rec.StorageKey) == src.SHA256 {
		t.Fatal("the damaged copy still matches the recorded checksum")
	}
	return &rec
}

// assertRestoreFailed checks a failed restore: an error matching one of wants (none:
// any error), a failed record, and a dropped partial clone.
func assertRestoreFailed(t *testing.T, env *mongoEnv, rec *models.RestoreRecord, err error, wants ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("restore of a damaged artifact succeeded (record %+v)", rec)
	}
	if len(wants) > 0 {
		matched := false
		for _, w := range wants {
			matched = matched || errors.Is(err, w)
		}
		if !matched {
			t.Fatalf("restore error %v; want one of %v", err, wants)
		}
	}
	if rec == nil || rec.Status != models.RestoreStatusFailed || rec.ErrorMessage == "" {
		t.Fatalf("restore record must be failed with a reason: %+v", rec)
	}
	t.Logf("failed as expected: %s", rec.ErrorMessage)
	if env.dbExists(t, rec.TargetDatabase) {
		env.dropDB(t, rec.TargetDatabase)
	}
}

// TestCorruptedBackupsFailLoudly damages stored archives (a changed byte, a
// truncation, a missing object, the wrong key) on every storage target and checks
// that the restore fails with a clear error, never reports success, and that the
// source database is never touched.
func TestCorruptedBackupsFailLoudly(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "cor")
	env.seedText(t, db, "notes", 300)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := env.Client.Database(db).Collection("notes").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "text", Value: 1}}, Options: options.Index().SetName(markerIndex),
	}); err != nil {
		t.Fatal(err)
	}
	source := env.snapshotDB(t, db)
	enc, dec := keyPair(t)
	yes := true

	for _, target := range storageTargets(t) {
		st := target.Storage
		t.Run(target.Name, func(t *testing.T) {
			plain := mustBackup(t, env, st, models.BackupOptions{Database: db})
			gz := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true})
			sealed := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true}, backup.WithEncryptor(enc))
			withKey := restore.WithDecryptor(dec)

			t.Run("changed metadata, streaming checksum", func(t *testing.T) {
				// The archive's per-collection CRC covers documents, not the metadata
				// (options, indexes): mongorestore accepts the renamed index, and only the
				// checksum computed while streaming reveals the damage.
				bad := damaged(t, st, plain, flipLetterAt(archiveOffset(t, st, plain.StorageKey, markerIndex)))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad)
				assertRestoreFailed(t, env, rec, err, restore.ErrChecksumMismatch)
				if rec.Verified || !strings.Contains(rec.ErrorMessage, "must not be trusted") {
					t.Fatalf("unexpected record: verified=%v message=%q", rec.Verified, rec.ErrorMessage)
				}
			})
			t.Run("changed document byte", func(t *testing.T) {
				bad := damaged(t, st, plain, flipLetterAt(plain.SizeBytes/2))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad)
				assertRestoreFailed(t, env, rec, err)
			})
			t.Run("changed byte, verify before restore", func(t *testing.T) {
				bad := damaged(t, st, plain, flipLetterAt(plain.SizeBytes/2))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{Verify: &yes}, bad)
				if env.dbExists(t, rec.TargetDatabase) {
					t.Fatalf("verification failed but %s was written", rec.TargetDatabase)
				}
				assertRestoreFailed(t, env, rec, err, restore.ErrChecksumMismatch)
			})
			t.Run("changed byte, gzip", func(t *testing.T) {
				bad := damaged(t, st, gz, flipByteAt(gz.SizeBytes/2))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad)
				assertRestoreFailed(t, env, rec, err)
			})
			t.Run("changed byte, encrypted", func(t *testing.T) {
				bad := damaged(t, st, sealed, flipByteAt(sealed.SizeBytes/2))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad, withKey)
				assertRestoreFailed(t, env, rec, err, encryption.ErrDecryptionFailed, restore.ErrChecksumMismatch)
			})
			t.Run("truncated", func(t *testing.T) {
				bad := damaged(t, st, plain, truncateAt(plain.SizeBytes*2/3))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad)
				assertRestoreFailed(t, env, rec, err)
			})
			t.Run("truncated, gzip", func(t *testing.T) {
				bad := damaged(t, st, gz, truncateAt(gz.SizeBytes-10))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad)
				assertRestoreFailed(t, env, rec, err)
			})
			t.Run("truncated, encrypted", func(t *testing.T) {
				bad := damaged(t, st, sealed, truncateAt(sealed.SizeBytes-100))
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, bad, withKey)
				assertRestoreFailed(t, env, rec, err, encryption.ErrDecryptionFailed, restore.ErrChecksumMismatch)
			})
			t.Run("missing object", func(t *testing.T) {
				gone := *plain
				gone.StorageKey = "missing/" + randomHex(t, 4) + ".archive"
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, &gone)
				assertRestoreFailed(t, env, rec, err, storage.ErrNotFound)
			})
			t.Run("wrong identity", func(t *testing.T) {
				_, other := keyPair(t)
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, sealed, restore.WithDecryptor(other))
				if env.dbExists(t, rec.TargetDatabase) {
					t.Fatalf("a wrong key must fail before %s is written", rec.TargetDatabase)
				}
				assertRestoreFailed(t, env, rec, err, encryption.ErrDecryptionFailed)
			})
			t.Run("no identity", func(t *testing.T) {
				rec, err := tryRestore(t, env, st, models.RestoreRequest{}, sealed)
				assertRestoreFailed(t, env, rec, err, encryption.ErrEncryptionKeyRequired)
			})
		})
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))
}

// TestRestoreNeverTouchesExistingDataWithoutOptIn checks the safe-clone default:
// requests that name an existing database without the explicit opt-in are refused
// and write nothing, and the default restore leaves the source untouched.
func TestRestoreNeverTouchesExistingDataWithoutOptIn(t *testing.T) {
	env := requireMongo(t)
	st := storageTargets(t)[0].Storage
	db := env.uniqueDB(t, "safe")
	env.seedText(t, db, "notes", 50)
	existing := db + "_existing"
	env.seed(t, existing, "orders", 20)
	source, other := env.snapshotDB(t, db), env.snapshotDB(t, existing)
	bkp := mustBackup(t, env, st, models.BackupOptions{Database: db, Gzip: true})
	no := false

	for name, req := range map[string]models.RestoreRequest{
		"target database without safe_clone false": {TargetDatabase: existing},
		"safe_clone false without confirmation":    {SafeClone: &no},
		"target database without confirmation":     {SafeClone: &no, TargetDatabase: existing},
		"drop without confirmation":                {SafeClone: &no, TargetDatabase: existing, DropTarget: true},
	} {
		rec, err := tryRestore(t, env, st, req, bkp)
		if !errors.Is(err, models.ErrInPlaceNotConfirmed) || rec != nil {
			t.Fatalf("%s: want ErrInPlaceNotConfirmed without a record, got %v / %+v", name, err, rec)
		}
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))
	assertSnapshotsEqual(t, other, env.snapshotDB(t, existing))

	// Default: a fresh clone, the source untouched.
	rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
	if !strings.HasPrefix(rst.TargetDatabase, db+"_rescue_") {
		t.Fatalf("default restore target %q is not a safe clone", rst.TargetDatabase)
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, rst.TargetDatabase))
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))

	// Confirmed in place without drop_target: every document collides, which is
	// reported as a failure instead of a successful no-op; the data is unchanged.
	rec, err := tryRestore(t, env, st, models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true}, bkp)
	if !errors.Is(err, restore.ErrDocumentsFailed) || rec.Status != models.RestoreStatusFailed {
		t.Fatalf("in-place restore over identical data: want ErrDocumentsFailed, got %v (%+v)", err, rec)
	}
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))

	// Confirmed in place with drop_target replaces the data with the backup.
	mustRestore(t, env, st, models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, DropTarget: true}, bkp)
	assertSnapshotsEqual(t, source, env.snapshotDB(t, db))
}
