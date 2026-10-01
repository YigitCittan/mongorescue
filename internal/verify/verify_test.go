package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func put(t *testing.T, st *storage.MockStorage, key string, data []byte) {
	t.Helper()
	if _, err := st.Save(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestArchivePlaintext(t *testing.T) {
	ctx := context.Background()
	st := storage.NewMockStorage()
	data := []byte(strings.Repeat("archive-bytes ", 1000))
	put(t, st, "db/a.archive", data)
	rec := &models.BackupRecord{ID: "b", StorageKey: "db/a.archive", SHA256: strings.ToUpper(sum(data)), SizeBytes: int64(len(data))}

	res := Archive(ctx, st, rec, Options{})
	if res.Status != models.VerificationOK || res.Err != nil || res.Bytes != int64(len(data)) || res.At.IsZero() {
		t.Fatalf("ok archive: %+v", res)
	}
	res.Apply(rec)
	if rec.Verification != models.VerificationOK || rec.VerifiedAt == nil || rec.VerificationError != "" {
		t.Fatalf("apply: %+v", rec)
	}

	// The stored object changed after it was recorded.
	tampered := append([]byte{}, data...)
	tampered[10] ^= 0xff
	put(t, st, "db/a.archive", tampered)
	res = Archive(ctx, st, rec, Options{})
	if res.Status != models.VerificationMismatch || !errors.Is(res.Err, ErrChecksumMismatch) {
		t.Fatalf("tampered archive: %+v", res)
	}
	res.Apply(rec)
	if rec.Verification != models.VerificationMismatch || !strings.Contains(rec.VerificationError, "sha256") {
		t.Fatalf("apply mismatch: %+v", rec)
	}

	// A truncated object is reported by its size.
	put(t, st, "db/a.archive", data[:100])
	if res = Archive(ctx, st, rec, Options{}); res.Status != models.VerificationMismatch || !strings.Contains(res.Err.Error(), "bytes") {
		t.Fatalf("truncated archive: %+v", res)
	}
}

func TestArchiveErrors(t *testing.T) {
	ctx := context.Background()
	st := storage.NewMockStorage()
	if res := Archive(ctx, st, &models.BackupRecord{StorageKey: "x"}, Options{}); res.Status != models.VerificationError || !errors.Is(res.Err, ErrNoChecksum) {
		t.Fatalf("no checksum: %+v", res)
	}
	if res := Archive(ctx, st, &models.BackupRecord{StorageKey: "gone", SHA256: "aa"}, Options{}); res.Status != models.VerificationError || !errors.Is(res.Err, storage.ErrNotFound) {
		t.Fatalf("missing object: %+v", res)
	}
	put(t, st, "k", []byte("data"))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if res := Archive(cancelled, st, &models.BackupRecord{StorageKey: "k", SHA256: sum([]byte("data"))}, Options{}); res.Status != models.VerificationError {
		t.Fatalf("cancelled: %+v", res)
	}
	if res := Archive(ctx, nil, &models.BackupRecord{StorageKey: "k", SHA256: "aa"}, Options{}); res.Status != models.VerificationError {
		t.Fatalf("nil storage: %+v", res)
	}
}

func TestArchiveDecryptCheck(t *testing.T) {
	ctx := context.Background()
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := encryption.NewX25519Encryptor([]string{recipient})
	var ct bytes.Buffer
	w, _ := enc.Encrypt(&ct)
	_, _ = io.WriteString(w, strings.Repeat("plain ", 5000))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st := storage.NewMockStorage()
	put(t, st, "db/a.archive.gz.age", ct.Bytes())
	rec := &models.BackupRecord{StorageKey: "db/a.archive.gz.age", SHA256: sum(ct.Bytes()), SizeBytes: int64(ct.Len())}

	dec, _ := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	if res := Archive(ctx, st, rec, Options{Decryptor: dec}); res.Status != models.VerificationOK || !res.Decrypted {
		t.Fatalf("decrypt check: %+v", res)
	}
	// Without a decryptor only the checksum is compared.
	if res := Archive(ctx, st, rec, Options{}); res.Status != models.VerificationOK || res.Decrypted {
		t.Fatalf("checksum only: %+v", res)
	}
	// An intact archive with the wrong key is an error, not a mismatch.
	other, _, _ := encryption.GenerateX25519()
	wrong, _ := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: other})
	if res := Archive(ctx, st, rec, Options{Decryptor: wrong}); res.Status != models.VerificationError || !errors.Is(res.Err, ErrDecryptCheck) {
		t.Fatalf("wrong key: %+v", res)
	}
	// A header-sniffed archive (record without its flag) is decrypted too.
	put(t, st, "db/b.archive", ct.Bytes())
	sniffed := &models.BackupRecord{StorageKey: "db/b.archive", SHA256: sum(ct.Bytes())}
	if res := Archive(ctx, st, sniffed, Options{Decryptor: dec}); res.Status != models.VerificationOK || !res.Decrypted {
		t.Fatalf("sniffed: %+v", res)
	}
}

func TestThrottle(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 3000)
	start := time.Now()
	n, err := io.Copy(io.Discard, throttle(context.Background(), bytes.NewReader(data), 10000))
	if err != nil || n != 3000 {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("3000 bytes at 10000 B/s took %s", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := io.Copy(io.Discard, throttle(ctx, bytes.NewReader(data), 10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled throttle: %v", err)
	}
	if r := bytes.NewReader(data); throttle(context.Background(), r, 0) != io.Reader(r) {
		t.Fatal("rate 0 must not wrap")
	}
}
