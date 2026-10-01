package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// payloadRunner returns a runner whose mongodump writes payload and succeeds.
func payloadRunner(payload []byte) ProcessRunner {
	return func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader(payload)), strings.NewReader(""), func() error { return nil }, nil
	}
}

// readBackStorage stores normally but serves reads through mutate, simulating an
// object that differs from what was written (bit rot, a broken gateway) or a read
// that fails.
type readBackStorage struct {
	*storage.MockStorage
	mutate  func([]byte) []byte
	readErr error
}

func (s *readBackStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	rc, err := s.MockStorage.Retrieve(ctx, key)
	if err != nil || s.mutate == nil {
		return rc, err
	}
	data, _ := io.ReadAll(rc)
	return io.NopCloser(bytes.NewReader(s.mutate(data))), nil
}

func TestVerifyAfterUploadOK(t *testing.T) {
	st := &readBackStorage{MockStorage: storage.NewMockStorage()}
	engine := NewEngine(st, "mongodb://localhost", WithRunner(payloadRunner([]byte("archive-payload"))), WithVerifyAfterUpload(true))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("run = %+v, %v", rec, err)
	}
	if rec.Verification != models.VerificationOK || rec.VerifiedAt == nil {
		t.Fatalf("verification = %q at %v", rec.Verification, rec.VerifiedAt)
	}
}

func TestVerifyAfterUploadMismatchFailsBackup(t *testing.T) {
	st := &readBackStorage{MockStorage: storage.NewMockStorage(), mutate: func(b []byte) []byte {
		out := append([]byte{}, b...)
		out[0] ^= 0xff
		return out
	}}
	engine := NewEngine(st, "mongodb://localhost", WithRunner(payloadRunner([]byte("archive-payload"))), WithVerifyAfterUpload(true))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if rec.Status != models.StatusFailed || rec.Verification != models.VerificationMismatch || rec.SHA256 != "" {
		t.Fatalf("record = %+v", rec)
	}
	if _, statErr := st.Stat(context.Background(), rec.StorageKey); !errors.Is(statErr, storage.ErrNotFound) {
		t.Fatalf("the damaged artifact must be deleted: %v", statErr)
	}
}

func TestVerifyAfterUploadErrorKeepsBackup(t *testing.T) {
	st := &readBackStorage{MockStorage: storage.NewMockStorage(), readErr: errors.New("gateway timeout")}
	engine := NewEngine(st, "mongodb://localhost", WithRunner(payloadRunner([]byte("archive-payload"))), WithVerifyAfterUpload(true))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("a verification that could not run must not fail the backup: %+v, %v", rec, err)
	}
	if rec.Verification != models.VerificationError || !strings.Contains(rec.VerificationError, "gateway timeout") {
		t.Fatalf("verification = %q (%s)", rec.Verification, rec.VerificationError)
	}
}

func TestVerifyAfterUploadOverride(t *testing.T) {
	st := &readBackStorage{MockStorage: storage.NewMockStorage(), readErr: errors.New("must not read")}
	cfg := RunConfig{Verify: true}
	engine := NewEngine(st, "mongodb://localhost", WithRunner(payloadRunner([]byte("x"))), WithRunConfig(func() RunConfig { return cfg }))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", Verify: models.VerifyOff})
	if err != nil || rec.Verification != "" {
		t.Fatalf("a job with verification off must not verify: %+v, %v", rec, err)
	}
	cfg.Verify = false
	st.readErr = nil
	rec, err = engine.Run(context.Background(), models.BackupOptions{Database: "shop", Verify: models.VerifyOn})
	if err != nil || rec.Verification != models.VerificationOK {
		t.Fatalf("a job with verification on must verify: %+v, %v", rec, err)
	}
}

func TestVerifyAfterUploadDecrypts(t *testing.T) {
	identity, recipient, _ := encryption.GenerateX25519()
	enc, _ := encryption.NewX25519Encryptor([]string{recipient})
	dec, _ := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	st := storage.NewMockStorage()
	engine := NewEngine(st, "mongodb://localhost", WithRunner(payloadRunner([]byte("secret archive"))),
		WithRunConfig(func() RunConfig { return RunConfig{Encryptor: enc, Verify: true, VerifyDecryptor: dec} }))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
	if err != nil || !rec.Encrypted || rec.Verification != models.VerificationOK {
		t.Fatalf("encrypted verify = %+v, %v", rec, err)
	}
}

// manifestSource returns successive manifests, counting the calls.
type manifestSource struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (m *manifestSource) capture(_ context.Context, _, database string) (*models.Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.fail {
		return nil, errors.New("not authorized on " + database)
	}
	orders := int64(10 + m.calls) // grows while the dump runs
	return &models.Manifest{Collections: []models.CollectionManifest{
		{Name: "orders", DocumentsMin: orders, DocumentsMax: orders, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
		{Name: "logs", DocumentsMin: 99, DocumentsMax: 99},
		{Name: "users", DocumentsMin: 3, DocumentsMax: 3},
	}}, nil
}

func TestManifestCapture(t *testing.T) {
	src := &manifestSource{}
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost", WithRunner(payloadRunner([]byte("x"))), WithManifestCapturer(src.capture))
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", ExcludeCollections: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 || !rec.HasManifest || rec.Manifest == nil {
		t.Fatalf("calls = %d, manifest = %+v", src.calls, rec.Manifest)
	}
	if rec.Manifest.Collection("logs") != nil {
		t.Fatal("excluded collections must not be in the manifest")
	}
	orders := rec.Manifest.Collection("orders")
	if orders == nil || orders.DocumentsMin != 11 || orders.DocumentsMax != 12 || len(orders.Indexes) != 1 {
		t.Fatalf("orders = %+v; the count range must span the dump", orders)
	}

	// A single included collection.
	rec, err = engine.Run(context.Background(), models.BackupOptions{Database: "shop", Collections: []string{"users"}})
	if err != nil || len(rec.Manifest.Collections) != 1 || rec.Manifest.Collections[0].Name != "users" {
		t.Fatalf("filtered manifest = %+v, %v", rec.Manifest, err)
	}

	// A failed capture leaves the backup without a manifest.
	src.fail = true
	rec, err = engine.Run(context.Background(), models.BackupOptions{Database: "shop"})
	if err != nil || rec.Status != models.StatusCompleted || rec.HasManifest || rec.Manifest != nil {
		t.Fatalf("failed capture = %+v, %v", rec, err)
	}
}
