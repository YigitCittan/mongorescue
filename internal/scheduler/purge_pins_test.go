package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPurgeKeepsTheReencryptedArchiveOfAPinnedBase proves that the purge honours a
// restore's pin on a base whose archive was re-encrypted meanwhile: the old
// archive the restore planned to read belongs to a tombstone with another ID, and
// it survives the purge (grace period 0) until the pin is released.
func TestPurgeKeepsTheReencryptedArchiveOfAPinnedBase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, mock := storetest.New(t), storage.NewMockStorage()
	newEncryptor := func() (*encryption.Encryptor, string) {
		t.Helper()
		id, rcpt, err := encryption.GenerateX25519()
		if err != nil {
			t.Fatal(err)
		}
		enc, err := encryption.NewX25519Encryptor([]string{rcpt})
		if err != nil {
			t.Fatal(err)
		}
		return enc, id
	}
	oldEnc, oldID := newEncryptor()
	newEnc, newID := newEncryptor()

	// Base backup B, encrypted with the old key.
	var buf bytes.Buffer
	w, err := oldEnc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "mongodump archive of B")
	_ = w.Close()
	sum := sha256.Sum256(buf.Bytes())
	const key = "_mongorescue/base/conn_a/rs0/2026/10/bkp_b.archive.gz.age"
	if _, err = mock.Save(ctx, key, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	base := &models.BackupRecord{ID: "bkp_b", Scope: models.ScopeInstance, PITRStreamID: "str_a", Status: models.StatusCompleted,
		StorageType: models.StorageLocal, StorageTargetID: "tgt_1", StorageKey: key, SizeBytes: int64(buf.Len()),
		SHA256: hex.EncodeToString(sum[:]), Encrypted: true, EncryptionMode: "x25519", StartedAt: time.Now().UTC()}
	if err = st.SaveBackupRecord(ctx, base); err != nil {
		t.Fatal(err)
	}

	// A restore pins B (its ID and the archive it reads), then B is re-encrypted.
	pins := pitr.NewPinSet(pitr.PinOptions{})
	pin := pins.Pin("restore of B", pitr.PinTarget{Bases: []string{base.ID},
		Archives: []pitr.ArchiveRef{{TargetID: base.StorageTargetID, Key: base.StorageKey}}})
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: newID + "\n" + oldID})
	if err != nil {
		t.Fatal(err)
	}
	svc := reencrypt.New(reencrypt.Config{
		Store:     st,
		Storage:   func(context.Context, string) (storage.Storage, error) { return mock, nil },
		Encryptor: func() *encryption.Encryptor { return newEnc },
		Decryptor: func() *encryption.Decryptor { return dec },
		Grace:     func() time.Duration { return 0 },
		Logger:    slog.New(slog.DiscardHandler),
	})
	svc.Start(ctx)
	if _, err = svc.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if rec, getErr := st.GetBackupRecord(ctx, base.ID); getErr != nil || rec.StorageKey == key {
		t.Fatalf("B was not re-encrypted: %+v, %v", rec, getErr)
	}

	purge := func() {
		t.Helper()
		run := purgeRun{now: time.Now().Add(time.Minute), grace: func() time.Duration { return 0 }, store: st,
			storages: fixedStorage(mock), logger: slog.New(slog.DiscardHandler), pins: pins}
		if _, purgeErr := run.run(ctx); purgeErr != nil {
			t.Fatal(purgeErr)
		}
	}
	purge()
	if _, err = mock.Stat(ctx, key); err != nil {
		t.Fatalf("the old archive the restore reads was purged under its pin: %v", err)
	}

	// Once the restore ends, the purge removes the tombstone's archive.
	pin.Release()
	purge()
	if _, err = mock.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old archive after the release: %v; want it purged", err)
	}
}
