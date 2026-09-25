package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestRunConfigAndStorageResolverApplyPerRun(t *testing.T) {
	stores := map[string]*storage.MockStorage{"stg_a": storage.NewMockStorage(), "stg_b": storage.NewMockStorage()}
	resolve := func(_ context.Context, id string) (storage.Storage, error) {
		s, ok := stores[id]
		if !ok {
			return nil, errors.New("no such target")
		}
		return s, nil
	}
	_, recipient, _ := encryption.GenerateX25519()
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	cfg := RunConfig{}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	e := NewEngine(nil, "mongodb://h", WithRunner(runner), WithStorageResolver(resolve),
		WithRunConfig(func() RunConfig { mu.Lock(); defer mu.Unlock(); return cfg }))
	ctx := context.Background()

	rec, err := e.Run(ctx, models.BackupOptions{Database: "db", StorageTargetID: "stg_a", StorageTargetName: "A"})
	if err != nil || rec.Encrypted || rec.StorageTargetID != "stg_a" || rec.StorageTargetName != "A" {
		t.Fatalf("plain run = %+v, %v", rec, err)
	}
	if _, err = stores["stg_a"].Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("artifact not on target a: %v", err)
	}

	// Encryption switched on between Prepare and Execute: the record follows the artifact.
	opts := models.BackupOptions{Database: "db", StorageTargetID: "stg_b"}
	prepared, err := e.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cfg = RunConfig{Encryptor: enc, Timeout: time.Hour}
	mu.Unlock()
	rec, err = e.Execute(ctx, opts, prepared)
	if err != nil || !rec.Encrypted || rec.EncryptionMode != "x25519" || !strings.HasSuffix(rec.StorageKey, encryption.FileExtension) {
		t.Fatalf("encrypted run = %+v, %v", rec, err)
	}
	if _, err = stores["stg_b"].Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("artifact not on target b under its record key: %v", err)
	}
	// And switched off again.
	prepared, _ = e.Prepare(opts)
	mu.Lock()
	cfg = RunConfig{}
	mu.Unlock()
	rec, _ = e.Execute(ctx, opts, prepared)
	if rec.Encrypted || strings.HasSuffix(rec.StorageKey, encryption.FileExtension) {
		t.Fatalf("record after disabling = %+v", rec)
	}

	rec, err = e.Run(ctx, models.BackupOptions{Database: "db", StorageTargetID: "stg_missing"})
	if err == nil || rec.Status != models.StatusFailed {
		t.Fatalf("unknown target = %+v, %v; want a failed record", rec, err)
	}
}

func TestBackupAndRestoreIDsAreUniqueWithinASecond(t *testing.T) {
	e := NewEngine(storage.NewMockStorage(), "mongodb://h")
	seen := map[string]bool{}
	for range 50 {
		rec, err := e.Prepare(models.BackupOptions{Database: strings.Repeat("d", 80)})
		if err != nil {
			t.Fatal(err)
		}
		if err := models.ValidateID(rec.ID); err != nil || seen[rec.ID] {
			t.Fatalf("backup id %q invalid or reused (%v)", rec.ID, err)
		}
		seen[rec.ID] = true
	}
}
