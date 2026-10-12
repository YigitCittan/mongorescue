//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// TestCrossRegionCopiesOnMinIO uses two MinIO buckets, one with Object Lock: the
// targets service records a region for both (the bucket location, or a label),
// the copy queue copies a PITR stream's oplog chunk from the plain bucket into the
// locked one with its checksum checked and its lock recorded, and the purge of
// the copy waits for that lock. Both targets share MinIO's key, which
// models.SameCredentials reports.
func TestCrossRegionCopiesOnMinIO(t *testing.T) {
	f := requireLockBucket(t)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	st := storetest.New(t)
	svc := targets.NewService(st, storage.NewForTarget, t.TempDir())
	input := func(name, bucket, prefix string, lock models.ObjectLockMode, region *string) targets.Input {
		days := 0
		if lock != "" {
			days = 1
		}
		return targets.Input{Name: name, Type: models.StorageS3, Region: region, S3: &models.S3Target{
			Endpoint: f.plain.Endpoint, Bucket: bucket, Prefix: prefix, AccessKeyID: f.plain.AccessKey,
			SecretAccessKey: f.plain.SecretKey, UsePathStyle: f.plain.UsePathStyle, ObjectLock: lock, RetentionDays: days,
		}}
	}
	plainPrefix := "it-dr/" + randomHex(t, 6) + "/"
	primary, err := svc.Create(ctx, input("dr-primary", f.plain.Bucket, plainPrefix, "", nil))
	if err != nil {
		t.Fatalf("create the primary target: %v", err)
	}
	regionB := "region-b"
	copyTarget, err := svc.Create(ctx, input("dr-copy", f.locked.Bucket, f.locked.Prefix, models.ObjectLockGovernance, &regionB))
	if err != nil {
		t.Fatalf("create the locked copy target: %v", err)
	}
	if copyTarget.DRRegion() != "region-b" || !copyTarget.ObjectLocked() {
		t.Fatalf("copy target = %+v; want region-b with Object Lock", copyTarget)
	}
	t.Logf("primary region from the bucket location: %q", primary.Region)
	if !models.SameCredentials(primary, copyTarget) {
		t.Fatal("two targets with one MinIO key do not share credentials")
	}

	src, err := svc.Storage(ctx, primary.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupPrefix(t, src, "") })
	body := bytes.Repeat([]byte("oplog chunk "), 2048)
	sum := sha256.Sum256(body)
	key := "_mongorescue/oplog/conn_dr/rs0/ch1/0000000100.0000000001-0000000160.0000000001.bson.gz.age"
	if _, err = src.Save(ctx, key, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateStream(ctx, &pitr.Stream{ID: "pst_dr", ConnectionID: "conn_dr", ReplicaSet: "rs0", TargetID: primary.ID,
		BaseCron: "@daily", BaseKeepCount: 7, ChunkSeconds: 60, CopyTargets: []string{copyTarget.ID}}); err != nil {
		t.Fatal(err)
	}
	if err = st.StartChain(ctx, "pst_dr", "ch1", pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}, Term: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	chunk := &pitr.Chunk{ID: "chk_dr", StreamID: "pst_dr", ChainID: "ch1", TargetID: primary.ID, StorageKey: key,
		From: pitr.Timestamp{T: 100, I: 1}, To: pitr.Timestamp{T: 160, I: 1}, FirstTerm: 1, LastTerm: 1, Entries: 1,
		SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Encrypted: true, EncryptionMode: "x25519"}
	if err = st.CommitChunk(ctx, chunk); err != nil {
		t.Fatal(err)
	}

	queue := copies.New(copies.Config{Store: st, Storages: svc.Storage, Logger: discardLogger})
	if err = queue.RunDue(ctx); err != nil {
		t.Fatalf("copy queue: %v", err)
	}
	list, err := st.ChunkCopiesOf(ctx, []string{"chk_dr"})
	if err != nil || len(list["chk_dr"]) != 1 {
		t.Fatalf("chunk copies = %+v, %v", list, err)
	}
	cp := list["chk_dr"][0]
	if cp.Status != models.CopyDone || !cp.SHA256OK || cp.VersionID == "" || cp.RetainUntil == nil {
		t.Fatalf("chunk copy = %+v; want done with a version and a retention", cp)
	}
	dst, err := svc.Storage(ctx, copyTarget.ID)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := storage.RetrieveVersion(ctx, dst, key, cp.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("copied chunk differs (%d bytes, %v)", len(got), err)
	}

	// Pruned on the primary: the copy is purged only once its own lock ends.
	if _, err = st.DeleteChunks(ctx, []string{"chk_dr"}, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = queue.RunDue(ctx); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	list, _ = st.ChunkCopiesOf(ctx, []string{"chk_dr"})
	if c := list["chk_dr"][0]; c.Status == models.CopyPurged {
		t.Fatalf("a locked copy was purged before its lock ended: %+v", c)
	}
	if _, err = storage.RetrieveVersion(ctx, dst, key, cp.VersionID); errors.Is(err, storage.ErrNotFound) {
		t.Fatal("the locked copy is gone")
	}
}
