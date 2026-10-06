package reencrypt_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

type keys struct {
	oldID, newID   string
	oldEnc, newEnc *encryption.Encryptor
}

func newKeys(t *testing.T) keys {
	t.Helper()
	var k keys
	var oldRcpt, newRcpt string
	var err error
	if k.oldID, oldRcpt, err = encryption.GenerateX25519(); err != nil {
		t.Fatal(err)
	}
	if k.newID, newRcpt, err = encryption.GenerateX25519(); err != nil {
		t.Fatal(err)
	}
	k.oldEnc, _ = encryption.NewX25519Encryptor([]string{oldRcpt})
	k.newEnc, _ = encryption.NewX25519Encryptor([]string{newRcpt})
	return k
}

func decryptor(t *testing.T, ids ...string) *encryption.Decryptor {
	t.Helper()
	d, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: strings.Join(ids, "\n")})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func plaintextOf(id string) string { return "mongodump archive of " + id }

// seedBackup stores an archive encrypted with enc and its record.
func seedBackup(t *testing.T, st *store.SQLiteStore, drv *storage.MockStorage, enc *encryption.Encryptor, id string) *models.BackupRecord {
	t.Helper()
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, plaintextOf(id))
	_ = w.Close()
	sum := sha256.Sum256(buf.Bytes())
	key := "app/2026/10/" + id + ".archive.gz.age"
	if _, err = drv.Save(context.Background(), key, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	rec := &models.BackupRecord{ID: id, Database: "app", Status: models.StatusCompleted, StorageType: models.StorageLocal,
		StorageTargetID: "tgt_1", StorageKey: key, SizeBytes: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:]),
		Encrypted: true, EncryptionMode: "x25519", StartedAt: time.Now().UTC()}
	if err = st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func service(st *store.SQLiteStore, drv storage.Storage, k keys, dec *encryption.Decryptor, after func(string) error) *reencrypt.Service {
	return reencrypt.New(reencrypt.Config{
		Store:     st,
		Storage:   func(context.Context, string) (storage.Storage, error) { return drv, nil },
		Encryptor: func() *encryption.Encryptor { return k.newEnc },
		Decryptor: func() *encryption.Decryptor { return dec },
		Grace:     func() time.Duration { return 24 * time.Hour },
		Logger:    slog.New(slog.DiscardHandler),
		AfterItem: after,
	})
}

// readPlain decrypts the archive of backup id with dec.
func readPlain(t *testing.T, st *store.SQLiteStore, drv storage.Storage, dec *encryption.Decryptor, id string) (string, error) {
	t.Helper()
	rec, err := st.GetBackupRecord(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	src, err := drv.Retrieve(context.Background(), rec.StorageKey)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	plain, err := dec.Decrypt(src)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(plain)
	return string(b), err
}

func waitDone(t *testing.T, svc *reencrypt.Service) *reencrypt.State {
	t.Helper()
	svc.Wait()
	st, err := svc.Status(context.Background())
	if err != nil || st == nil {
		t.Fatalf("status = %v, %v", st, err)
	}
	return st
}

func TestReencryptSwapsArchivesAndKeepsTheOldOnesInGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, drv, k := storetest.New(t), storage.NewMockStorage(), newKeys(t)
	old := seedBackup(t, st, drv, k.oldEnc, "bkp_a")
	svc := service(st, drv, k, decryptor(t, k.newID, k.oldID), nil)
	svc.Start(ctx)
	if _, err := svc.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	job := waitDone(t, svc)
	if job.Status != reencrypt.StatusCompleted || job.Done != 1 {
		t.Fatalf("job = %+v", job)
	}
	// Only the new key is needed now.
	if got, err := readPlain(t, st, drv, decryptor(t, k.newID), "bkp_a"); err != nil || got != plaintextOf("bkp_a") {
		t.Fatalf("new archive = %q, %v", got, err)
	}
	rec, _ := st.GetBackupRecord(ctx, "bkp_a")
	if rec.StorageKey == old.StorageKey || rec.SHA256 == old.SHA256 {
		t.Fatal("the record was not swapped")
	}
	// The old archive stays, owned by a deleted tombstone, until the grace period ends.
	if _, err := drv.Stat(ctx, old.StorageKey); err != nil {
		t.Fatalf("old archive: %v", err)
	}
	list, _ := st.ListBackupRecords(ctx, "")
	var tomb *models.BackupRecord
	for _, r := range list {
		if r.ID != "bkp_a" {
			tomb = r
		}
	}
	if tomb == nil || tomb.Status != models.StatusDeleted || tomb.StorageKey != old.StorageKey || tomb.PurgeAfter == nil {
		t.Fatalf("tombstone = %+v", tomb)
	}
}

func TestReencryptResumesAfterAnInterruption(t *testing.T) {
	ctx := context.Background()
	st, drv, k := storetest.New(t), storage.NewMockStorage(), newKeys(t)
	seedBackup(t, st, drv, k.oldEnc, "bkp_a")
	seedBackup(t, st, drv, k.oldEnc, "bkp_b")
	dec := decryptor(t, k.newID, k.oldID)
	stop := errors.New("crash")
	first := service(st, drv, k, dec, func(string) error { return stop })
	firstCtx, cancel := context.WithCancel(ctx)
	first.Start(firstCtx)
	if _, err := first.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	job := waitDone(t, first)
	cancel()
	if job.Status != reencrypt.StatusRunning || len(job.Finished) != 1 {
		t.Fatalf("after the interruption: %+v", job)
	}
	doneID := job.Finished[0]
	before, _ := st.GetBackupRecord(ctx, doneID)

	second := service(st, drv, k, dec, nil)
	second.Start(ctx)
	job = waitDone(t, second)
	if job.Status != reencrypt.StatusCompleted || job.Done != 2 {
		t.Fatalf("after resuming: %+v", job)
	}
	after, _ := st.GetBackupRecord(ctx, doneID)
	if after.StorageKey != before.StorageKey {
		t.Fatal("a finished backup was re-encrypted again")
	}
	for _, id := range []string{"bkp_a", "bkp_b"} {
		if got, err := readPlain(t, st, drv, decryptor(t, k.newID), id); err != nil || got != plaintextOf(id) {
			t.Fatalf("%s = %q, %v", id, got, err)
		}
	}
}

func TestReencryptRemovesAHalfWrittenArchive(t *testing.T) {
	ctx := context.Background()
	st, drv, k := storetest.New(t), storage.NewMockStorage(), newKeys(t)
	rec := seedBackup(t, st, drv, k.oldEnc, "bkp_a")
	partial := "app/2026/10/bkp_a-rk1.archive.gz.age"
	if _, err := drv.Save(ctx, partial, strings.NewReader("truncated")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveIntegrityState(ctx, "encryption.reencrypt", reencrypt.State{ID: "reenc_x", Status: reencrypt.StatusRunning,
		Current: &reencrypt.Item{BackupID: "bkp_a", TargetID: "tgt_1", OldKey: rec.StorageKey, NewKey: partial}}); err != nil {
		t.Fatal(err)
	}
	svc := service(st, drv, k, decryptor(t, k.newID, k.oldID), nil)
	svc.Start(ctx)
	job := waitDone(t, svc)
	if job.Status != reencrypt.StatusCompleted || job.Done != 1 {
		t.Fatalf("job = %+v", job)
	}
	if _, err := drv.Stat(ctx, partial); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("partial archive = %v; want it removed", err)
	}
}

func TestReencryptRefusesATamperedArchive(t *testing.T) {
	ctx := context.Background()
	st, drv, k := storetest.New(t), storage.NewMockStorage(), newKeys(t)
	rec := seedBackup(t, st, drv, k.oldEnc, "bkp_a")
	rec.SHA256 = strings.Repeat("0", 64)
	if err := st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	svc := service(st, drv, k, decryptor(t, k.newID, k.oldID), nil)
	svc.Start(ctx)
	if _, err := svc.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	job := waitDone(t, svc)
	if job.Failed != 1 || job.Status != reencrypt.StatusFailed {
		t.Fatalf("job = %+v", job)
	}
	after, _ := st.GetBackupRecord(ctx, "bkp_a")
	if after.StorageKey != rec.StorageKey {
		t.Fatal("a backup that failed its hash check was swapped")
	}
}
