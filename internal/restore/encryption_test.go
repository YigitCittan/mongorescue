package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

const testScryptWorkFactor = 10

func newKeyPair(t *testing.T) (identity, recipient string) {
	t.Helper()
	id, r, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	return id, r
}

func mustDecryptor(t *testing.T, cfg encryption.DecryptorConfig) *encryption.Decryptor {
	t.Helper()
	dec, err := encryption.NewDecryptor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

func sealPayload(t *testing.T, enc *encryption.Encryptor, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// capturingRunner records stdin contents and args of the (mock) mongorestore run.
type capturingRunner struct {
	called bool
	args   []string
	stdin  []byte
	err    error
}

func (c *capturingRunner) run(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	c.called = true
	c.args = args
	c.stdin, c.err = io.ReadAll(stdin)
	waitErr := c.err
	return strings.NewReader(""), func() error { return waitErr }, nil
}

func storeEncrypted(t *testing.T, store *storage.MockStorage, enc *encryption.Encryptor, payload []byte) *models.BackupRecord {
	t.Helper()
	key := "shop/2026/09/bkp_shop_20260924_120000.archive.gz.age"
	if _, err := store.Save(context.Background(), key, bytes.NewReader(sealPayload(t, enc, payload))); err != nil {
		t.Fatal(err)
	}
	return &models.BackupRecord{
		ID:             "bkp_shop_20260924_120000",
		Database:       "shop",
		StorageKey:     key,
		Encrypted:      true,
		EncryptionMode: string(enc.Mode()),
	}
}

func TestRestoreEncryptedRoundTrip(t *testing.T) {
	id1, r1 := newKeyPair(t)
	id2, r2 := newKeyPair(t)
	x25519, _ := encryption.NewX25519Encryptor([]string{r1, r2})
	scrypt, _ := encryption.NewScryptEncryptor("restore-pass", testScryptWorkFactor)

	cases := []struct {
		name string
		enc  *encryption.Encryptor
		dec  encryption.DecryptorConfig
	}{
		{"x25519 first recipient", x25519, encryption.DecryptorConfig{Identity: id1}},
		{"x25519 second recipient", x25519, encryption.DecryptorConfig{Identity: id2}},
		{"scrypt", scrypt, encryption.DecryptorConfig{Passphrase: "restore-pass"}},
	}
	payload := bytes.Repeat([]byte("bson-archive-"), 30_000)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewMockStorage()
			src := storeEncrypted(t, store, tc.enc, payload)
			runner := &capturingRunner{}
			engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithDecryptor(mustDecryptor(t, tc.dec)))

			record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if record.Status != models.RestoreStatusCompleted {
				t.Fatalf("status = %s", record.Status)
			}
			if !bytes.Equal(runner.stdin, payload) {
				t.Fatal("mongorestore did not receive the decrypted archive")
			}
			if !slices.Contains(runner.args, "--gzip") {
				t.Fatalf("expected --gzip for .archive.gz.age key, got %v", runner.args)
			}
		})
	}
}

func TestRestoreEncryptedMissingKey(t *testing.T) {
	_, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	store := storage.NewMockStorage()
	src := storeEncrypted(t, store, enc, []byte("data"))
	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))

	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, encryption.ErrEncryptionKeyRequired) {
		t.Fatalf("expected ErrEncryptionKeyRequired, got %v", err)
	}
	if runner.called {
		t.Fatal("mongorestore must not start without a decryption key")
	}
	if record == nil || record.Status != models.RestoreStatusFailed {
		t.Fatalf("expected failed record, got %+v", record)
	}
}

func TestRestoreEncryptedWrongKey(t *testing.T) {
	_, r := newKeyPair(t)
	wrongID, _ := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	store := storage.NewMockStorage()
	src := storeEncrypted(t, store, enc, []byte("data"))
	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: wrongID, Passphrase: "pw-secret"})))

	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, encryption.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
	if runner.called {
		t.Fatal("mongorestore must not start when the header cannot be decrypted")
	}
	for _, secret := range []string{wrongID, "pw-secret"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(record.ErrorMessage, secret) {
			t.Fatal("error leaks key material")
		}
	}
}

func TestRestoreEncryptedTamperedStream(t *testing.T) {
	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	store := storage.NewMockStorage()
	ciphertext := sealPayload(t, enc, bytes.Repeat([]byte("z"), 300*1024))
	ciphertext[len(ciphertext)-100] ^= 0x01
	key := "db/2026/09/bkp.archive.age"
	_, _ = store.Save(context.Background(), key, bytes.NewReader(ciphertext))
	src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, Encrypted: true}

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, encryption.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
	if record.Status != models.RestoreStatusFailed {
		t.Fatalf("status = %s", record.Status)
	}
	if slices.Contains(runner.args, "--gzip") {
		t.Fatalf("unexpected --gzip for plain .archive.age key: %v", runner.args)
	}
}

func TestRestoreLegacyUnencryptedWithDecryptorConfigured(t *testing.T) {
	id, _ := newKeyPair(t)
	store := storage.NewMockStorage()
	payload := []byte("legacy-plaintext-archive")
	key := "db/2026/09/bkp_db_20260101_000000.archive.gz"
	_, _ = store.Save(context.Background(), key, bytes.NewReader(payload))
	// Records written before encryption support have no encryption fields.
	src := &models.BackupRecord{ID: "bkp_db_20260101_000000", Database: "db", StorageKey: key}

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
	if _, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(runner.stdin, payload) || !slices.Contains(runner.args, "--gzip") {
		t.Fatalf("legacy restore changed: stdin=%q args=%v", runner.stdin, runner.args)
	}
}

// streamingStorage serves a ciphertext generated on the fly, so the test itself
// never holds the full archive in memory.
type streamingStorage struct {
	storage.Storage
	open func() io.ReadCloser
}

func (s *streamingStorage) Retrieve(context.Context, string) (io.ReadCloser, error) {
	return s.open(), nil
}

// patternReader generates n bytes on the fly without allocating.
type patternReader struct{ remaining int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.remaining {
		b = b[:p.remaining]
	}
	for i := range b {
		b[i] = byte(i)
	}
	p.remaining -= int64(len(b))
	return len(b), nil
}

func TestRestoreEncryptedLargePayloadStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("large payload test skipped in -short mode")
	}
	const payloadSize = 50 << 20
	const maxHeapGrowth = 16 << 20

	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	store := &streamingStorage{open: func() io.ReadCloser {
		pr, pw := io.Pipe()
		go func() {
			w, err := enc.Encrypt(pw)
			if err == nil {
				_, err = io.Copy(w, &patternReader{remaining: payloadSize})
			}
			if err == nil {
				err = w.Close()
			}
			_ = pw.CloseWithError(err)
		}()
		return pr
	}}

	var peakHeap uint64
	var received int64
	runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		buf := make([]byte, 32*1024)
		var ms runtime.MemStats
		var sinceSample int64
		var readErr error
		for {
			n, err := stdin.Read(buf)
			received += int64(n)
			sinceSample += int64(n)
			if sinceSample >= 4<<20 {
				sinceSample = 0
				runtime.ReadMemStats(&ms)
				peakHeap = max(peakHeap, ms.HeapInuse)
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				break
			}
		}
		return strings.NewReader(""), func() error { return readErr }, nil
	}

	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
	src := &models.BackupRecord{ID: "bkp_big", Database: "big", StorageKey: "big/bkp.archive.age", Encrypted: true}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if received != payloadSize {
		t.Fatalf("mongorestore received %d bytes, want %d", received, payloadSize)
	}
	t.Logf("baseline heap %d KiB, peak heap %d KiB", before.HeapInuse>>10, peakHeap>>10)
	if peakHeap > before.HeapInuse && peakHeap-before.HeapInuse > maxHeapGrowth {
		t.Fatalf("heap grew by %d MiB while restoring %d MiB: payload is being buffered",
			(peakHeap-before.HeapInuse)>>20, payloadSize>>20)
	}
}
