package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

func staticRunner(payload []byte) ProcessRunner {
	return func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader(payload)), strings.NewReader(""), func() error { return nil }, nil
	}
}

// assertNoGoroutineLeak waits for the goroutine count to settle back to baseline.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutine leak: %d running, baseline %d\n%s", n, baseline, buf[:runtime.Stack(buf, true)])
	}
}

func TestBackupEncryptedRoundTrip(t *testing.T) {
	id1, r1 := newKeyPair(t)
	id2, r2 := newKeyPair(t)
	x25519, err := encryption.NewX25519Encryptor([]string{r1, r2})
	if err != nil {
		t.Fatal(err)
	}
	scrypt, err := encryption.NewScryptEncryptor("backup-passphrase", testScryptWorkFactor)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		enc        *encryption.Encryptor
		mode       string
		decryptors []encryption.DecryptorConfig
	}{
		{"x25519 multi-recipient", x25519, "x25519", []encryption.DecryptorConfig{{Identity: id1}, {Identity: id2}}},
		{"scrypt passphrase", scrypt, "scrypt", []encryption.DecryptorConfig{{Passphrase: "backup-passphrase"}}},
	}

	payload := bytes.Repeat([]byte("mongodump-archive-"), 20_000) // ~360 KB, several age chunks
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := storage.NewMockStorage()
			engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(staticRunner(payload)), WithEncryptor(tc.enc))

			record, err := engine.Run(ctx, models.BackupOptions{Database: "shop", Gzip: true})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !record.Encrypted || record.EncryptionMode != tc.mode {
				t.Fatalf("record encryption fields = %v/%q", record.Encrypted, record.EncryptionMode)
			}
			if !strings.HasSuffix(record.StorageKey, ".archive.gz.age") {
				t.Fatalf("unexpected storage key %q", record.StorageKey)
			}

			stored, err := store.Retrieve(ctx, record.StorageKey)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, _ := io.ReadAll(stored)
			_ = stored.Close()

			sum := sha256.Sum256(ciphertext)
			if record.SHA256 != hex.EncodeToString(sum[:]) || record.SizeBytes != int64(len(ciphertext)) {
				t.Fatal("size and checksum must describe the stored ciphertext")
			}
			if bytes.Contains(ciphertext, []byte("mongodump-archive-")) {
				t.Fatal("stored artifact contains plaintext")
			}

			for i, dc := range tc.decryptors {
				dec, err := encryption.NewDecryptor(dc)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := dec.Decrypt(bytes.NewReader(ciphertext))
				if err != nil {
					t.Fatalf("decryptor #%d: %v", i, err)
				}
				got, err := io.ReadAll(plain)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("decryptor #%d: plaintext mismatch (err=%v)", i, err)
				}
			}
		})
	}
}

func TestBackupUnencryptedKeepsNaming(t *testing.T) {
	engine := NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(staticRunner([]byte("x"))))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "db", Gzip: true})
	if err != nil {
		t.Fatal(err)
	}
	if record.Encrypted || record.EncryptionMode != "" || !strings.HasSuffix(record.StorageKey, ".archive.gz") {
		t.Fatalf("unexpected plaintext record: key=%q encrypted=%v", record.StorageKey, record.Encrypted)
	}
}

// ctxBlockingReader yields n bytes and then blocks until ctx is cancelled.
type ctxBlockingReader struct {
	ctx     context.Context
	n       int
	started chan struct{}
	once    sync.Once
}

func (r *ctxBlockingReader) Read(p []byte) (int, error) {
	if r.n > 0 {
		if len(p) > r.n {
			p = p[:r.n]
		}
		for i := range p {
			p[i] = 'a'
		}
		r.n -= len(p)
		return len(p), nil
	}
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestBackupEncryptedContextCancelNoLeak(t *testing.T) {
	_, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &ctxBlockingReader{ctx: ctx, n: 1 << 20, started: make(chan struct{})}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(src), strings.NewReader(""), func() error { return ctx.Err() }, nil
	}
	store := storage.NewMockStorage()
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner), WithEncryptor(enc))

	go func() {
		<-src.started
		cancel()
	}()
	record, err := engine.Run(ctx, models.BackupOptions{Database: "db"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if record.Status != models.StatusFailed {
		t.Fatalf("expected failed status, got %s", record.Status)
	}
	if _, err := store.Retrieve(context.Background(), record.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("no artifact must be stored for a cancelled backup, got %v", err)
	}
	assertNoGoroutineLeak(t, baseline)
}

// failingStorage consumes a few bytes and then aborts the upload.
type failingStorage struct {
	storage.Storage
	err error
}

func (f *failingStorage) Save(_ context.Context, _ string, r io.Reader) (*models.StorageObject, error) {
	_, _ = io.ReadFull(r, make([]byte, 16))
	return nil, f.err
}

func TestBackupEncryptedStorageFailureUnblocksProducer(t *testing.T) {
	_, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	baseline := runtime.NumGoroutine()

	// mongodump output that never ends: the encryption goroutine blocks reading it.
	stdoutR, stdoutW := io.Pipe()
	defer stdoutW.Close()
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return stdoutR, strings.NewReader(""), func() error { return nil }, nil
	}
	uploadErr := errors.New("s3: upload aborted")
	engine := NewEngine(&failingStorage{Storage: storage.NewMockStorage(), err: uploadErr}, "mongodb://localhost:27017",
		WithRunner(runner), WithEncryptor(enc))

	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(context.Background(), models.BackupOptions{Database: "db"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, uploadErr) {
			t.Fatalf("expected storage error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run deadlocked after storage failure")
	}
	assertNoGoroutineLeak(t, baseline)
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
		b[i] = byte(i*31 + int(p.remaining))
	}
	p.remaining -= int64(len(b))
	return len(b), nil
}

// samplingStorage discards data while sampling peak heap usage during the upload.
type samplingStorage struct {
	storage.Storage
	peakHeap uint64
	written  int64
}

func (s *samplingStorage) Save(_ context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	buf := make([]byte, 32*1024)
	var ms runtime.MemStats
	var sinceSample int64
	for {
		n, err := r.Read(buf)
		s.written += int64(n)
		sinceSample += int64(n)
		if sinceSample >= 4<<20 {
			sinceSample = 0
			runtime.ReadMemStats(&ms)
			s.peakHeap = max(s.peakHeap, ms.HeapInuse)
		}
		if errors.Is(err, io.EOF) {
			return &models.StorageObject{Key: key, SizeBytes: s.written}, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s *samplingStorage) Delete(context.Context, string) error { return nil }

func TestBackupEncryptedLargePayloadStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("large payload test skipped in -short mode")
	}
	const payloadSize = 50 << 20
	const maxHeapGrowth = 16 << 20

	_, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(&patternReader{remaining: payloadSize}), strings.NewReader(""), func() error { return nil }, nil
	}
	store := &samplingStorage{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner), WithEncryptor(enc))

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "big"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.SizeBytes <= payloadSize || store.written != record.SizeBytes {
		t.Fatalf("unexpected sizes: record=%d written=%d", record.SizeBytes, store.written)
	}
	t.Logf("baseline heap %d KiB, peak heap %d KiB", before.HeapInuse>>10, store.peakHeap>>10)
	if store.peakHeap > before.HeapInuse && store.peakHeap-before.HeapInuse > maxHeapGrowth {
		t.Fatalf("heap grew by %d MiB while streaming %d MiB: payload is being buffered",
			(store.peakHeap-before.HeapInuse)>>20, payloadSize>>20)
	}
}
