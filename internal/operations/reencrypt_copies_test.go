package operations_test

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

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestReencryptionMovesCopiesToTheNewArchive proves that re-encrypting a backup
// with a copy hands the old copy to the tombstone and queues a new copy of the new
// archive: until it is made no copy is usable, then a restore can read it, and
// the purge after the grace period removes the old copy and keeps the new one.
func TestReencryptionMovesCopiesToTheNewArchive(t *testing.T) {
	ctx := context.Background()
	oldID, oldRcpt, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	newID, newRcpt, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	oldEnc, _ := encryption.NewX25519Encryptor([]string{oldRcpt})
	newEnc, _ := encryption.NewX25519Encryptor([]string{newRcpt})
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: newID + "\n" + oldID})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w, _ := oldEnc.Encrypt(&buf)
	_, _ = io.WriteString(w, "mongodump archive")
	_ = w.Close()
	sum := sha256.Sum256(buf.Bytes())
	const oldKey = "app/2026/10/bkp_rot.archive.gz.age"
	primary, copyDrv := storage.NewMockStorage(), storage.NewMockStorage()
	for _, d := range []*storage.MockStorage{primary, copyDrv} {
		if _, err = d.Save(ctx, oldKey, bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	drivers := map[string]storage.Storage{"tgt_p": primary, "tgt_c": copyDrv}
	resolve := func(_ context.Context, id string) (storage.Storage, error) {
		if d, ok := drivers[id]; ok {
			return d, nil
		}
		return nil, errors.New("unknown target")
	}
	st := storetest.New(t)
	rec := &models.BackupRecord{ID: "bkp_rot", Database: "app", ConnectionID: "conn_a", Status: models.StatusCompleted,
		StorageTargetID: "tgt_p", StorageKey: oldKey, SizeBytes: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:]),
		Encrypted: true, EncryptionMode: "x25519", StartedAt: time.Now().UTC()}
	rec.PlanCopies([]models.CopyTarget{{ID: "tgt_c", Name: "offsite"}}, "")
	rec.Copies[0].Status, rec.Copies[0].SHA256, rec.Copies[0].SHA256OK = models.CopyDone, rec.SHA256, true
	if err = st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Rotate.
	grace := 24 * time.Hour
	re := reencrypt.New(reencrypt.Config{
		Store: st, Storage: resolve, Logger: slog.New(slog.DiscardHandler),
		Encryptor: func() *encryption.Encryptor { return newEnc },
		Decryptor: func() *encryption.Decryptor { return dec },
		Grace:     func() time.Duration { return grace },
	})
	re.Start(ctx)
	if _, err = re.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	re.Wait()
	live, _ := st.GetBackupRecord(ctx, "bkp_rot")
	if live.StorageKey == oldKey || len(live.Copies) != 1 {
		t.Fatalf("not re-encrypted: %+v", live)
	}
	if c := live.Copies[0]; c.Status != models.CopyPending || c.StorageKey != live.StorageKey || live.CopyUsable(&c) {
		t.Fatalf("the copy must be queued again under the new key: %+v", c)
	}

	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := operations.New(operations.Config{
		Store: st, Backup: backup.NewEngine(storage.NewMockStorage(), ""), Runs: manager,
		Restore:     &completingEngine{prep: restore.NewEngine(nil, ""), canDecrypt: true},
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://db.internal:27017"}},
		Storage:     resolve,
	})
	// No copy of the new archive yet: reading the copy is refused.
	if _, err = svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_rot", SourceTargetID: "tgt_c"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("restore from the stale copy = %v, want ErrInvalid", err)
	}
	queue := copies.New(copies.Config{Store: st, Storages: resolve, Logger: slog.New(slog.DiscardHandler)})
	if err = queue.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	live, _ = st.GetBackupRecord(ctx, "bkp_rot")
	if c := live.Copies[0]; c.Status != models.CopyDone || c.SHA256 != live.SHA256 {
		t.Fatalf("new copy = %+v", c)
	}
	rst, err := svc.StartRestore(ctx, models.RestoreRequest{BackupID: "bkp_rot", SourceTargetID: "tgt_c"})
	if err != nil || rst.SourceTargetID != "tgt_c" {
		t.Fatalf("restore from the new copy = %+v, %v", rst, err)
	}

	// After the grace period the purge removes the old archive and its old copy.
	if _, err = scheduler.PurgeDeleted(ctx, time.Now().Add(grace+time.Hour), grace, st, resolve, nil, nil); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]storage.Storage{"primary": primary, "copy": copyDrv} {
		if _, statErr := d.Stat(ctx, oldKey); !errors.Is(statErr, storage.ErrNotFound) {
			t.Fatalf("the old archive on the %s target must be purged: %v", name, statErr)
		}
		if _, statErr := d.Stat(ctx, live.StorageKey); statErr != nil {
			t.Fatalf("the new archive on the %s target must survive: %v", name, statErr)
		}
	}
	if !strings.HasSuffix(live.StorageKey, encryption.FileExtension) {
		t.Fatalf("new key %q", live.StorageKey)
	}
}
