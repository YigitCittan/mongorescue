package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// fakeArchive returns bytes that start like an uncompressed mongodump archive.
func fakeArchive(body string) []byte {
	return append(slices.Clone(archiveMagic), body...)
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func saveArtifact(t *testing.T, store storage.Storage, key string, data []byte) {
	t.Helper()
	if _, err := store.Save(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreDetectsCompressionFromContent covers backups whose key does not follow
// the naming scheme: a custom target_key keeps no ".gz" even though mongodump ran
// with --gzip, so the archive signature must decide.
func TestRestoreDetectsCompressionFromContent(t *testing.T) {
	archive := fakeArchive("collections")

	cases := []struct {
		name     string
		key      string
		stored   []byte
		wantGzip bool
	}{
		{"custom key, gzip content", "custom/nightly-dump", gzipBytes(t, archive), true},
		{"custom .archive key, gzip content", "custom/shop.archive", gzipBytes(t, archive), true},
		{"gz key, plain archive content", "custom/shop.archive.gz", archive, false},
		{"unknown content, gz key", "db/2026/09/bkp.archive.gz", []byte("opaque"), true},
		{"unknown content, plain key", "db/2026/09/bkp.archive", []byte("opaque"), false},
		{"empty artifact, gz key", "db/2026/09/empty.archive.gz", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewMockStorage()
			saveArtifact(t, store, tc.key, tc.stored)
			src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: tc.key, SHA256: sha256Hex(tc.stored)}
			runner := &capturingRunner{}
			engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
			if _, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := slices.Contains(runner.args, "--gzip"); got != tc.wantGzip {
				t.Fatalf("--gzip = %v; want %v (args %v)", got, tc.wantGzip, runner.args)
			}
			if !bytes.Equal(runner.stdin, tc.stored) {
				t.Fatal("mongorestore must receive the stored bytes unchanged")
			}
		})
	}
}

// TestRestoreNeverTreatsCiphertextAsPlaintext covers records that lost their
// encryption flag: the .age key or the age header still selects decryption, and
// without a key the restore stops before mongorestore starts.
func TestRestoreNeverTreatsCiphertextAsPlaintext(t *testing.T) {
	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	archive := gzipBytes(t, fakeArchive("secret-documents"))
	ciphertext := sealPayload(t, enc, archive)

	for _, key := range []string{"db/2026/09/bkp.archive.gz.age", "custom/unflagged-dump"} {
		for _, verify := range []bool{false, true} {
			name := key
			if verify {
				name += "/verify"
			}
			t.Run(name+"/no key", func(t *testing.T) {
				store := storage.NewMockStorage()
				saveArtifact(t, store, key, ciphertext)
				src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, SHA256: sha256Hex(ciphertext)}
				runner := &capturingRunner{}
				engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
				record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, Verify: &verify}, src)
				if !errors.Is(err, encryption.ErrEncryptionKeyRequired) {
					t.Fatalf("Run = %v; want ErrEncryptionKeyRequired", err)
				}
				if runner.called {
					t.Fatal("ciphertext must never be streamed to mongorestore")
				}
				if record.Status != models.RestoreStatusFailed || !strings.Contains(record.ErrorMessage, "Settings → Encryption") {
					t.Fatalf("record = %+v; want an actionable failure", record)
				}
			})
			t.Run(name+"/with key", func(t *testing.T) {
				store := storage.NewMockStorage()
				saveArtifact(t, store, key, ciphertext)
				src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, SHA256: sha256Hex(ciphertext)}
				runner := &capturingRunner{}
				engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
					WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
				record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, Verify: &verify}, src)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if record.Verified != verify || !bytes.Equal(runner.stdin, archive) || !slices.Contains(runner.args, "--gzip") {
					t.Fatalf("verified=%v args=%v; mongorestore must receive the decrypted gzip archive", record.Verified, runner.args)
				}
			})
		}
	}
}

// TestRestoreTruncatedCiphertextFailsWithoutPartialSuccess cuts an encrypted backup
// at several offsets. Every cut must fail with ErrDecryptionFailed; cuts inside the
// header or the first chunk fail before mongorestore starts even without
// verification, and with verification no cut ever reaches mongorestore.
func TestRestoreTruncatedCiphertextFailsWithoutPartialSuccess(t *testing.T) {
	const chunk = 64 * 1024
	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	full := sealPayload(t, enc, bytes.Repeat([]byte("d"), 3*chunk+10))
	macLine := bytes.Index(full, []byte("\n--- ")) + 1
	headerLen := macLine + bytes.IndexByte(full[macLine:], '\n') + 1 // through the "--- <MAC>" line
	payloadStart := headerLen + 16                                   // after the stream nonce
	sealed := chunk + 16                                             // one chunk and its tag

	cuts := []struct {
		name       string
		length     int
		earlyNoVer bool // detected before mongorestore starts even without verification
	}{
		{"inside the header", headerLen / 2, true},
		{"after the header", headerLen, true},
		{"inside the first chunk", payloadStart + chunk/2, true},
		{"at the first chunk boundary", payloadStart + sealed, false},
		{"inside the last chunk", len(full) - 5, false},
		{"tag of the last chunk missing", len(full) - 16, false},
	}
	for _, c := range cuts {
		for _, verify := range []bool{false, true} {
			t.Run(c.name+map[bool]string{false: "", true: "/verify"}[verify], func(t *testing.T) {
				truncated := full[:c.length]
				store := storage.NewMockStorage()
				key := "db/2026/09/bkp.archive.age"
				saveArtifact(t, store, key, truncated)
				src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, Encrypted: true, SHA256: sha256Hex(truncated)}
				runner := &capturingRunner{}
				engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
					WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
				record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, Verify: &verify}, src)
				if !errors.Is(err, encryption.ErrDecryptionFailed) {
					t.Fatalf("Run = %v; want ErrDecryptionFailed", err)
				}
				if record.Status != models.RestoreStatusFailed {
					t.Fatalf("status = %s; want failed", record.Status)
				}
				if early := verify || c.earlyNoVer; early && runner.called {
					t.Fatal("mongorestore must not start for this cut")
				}
				if runner.called && !strings.Contains(record.ErrorMessage, "partial data may have been applied") {
					t.Fatalf("a late failure must warn about partial data: %q", record.ErrorMessage)
				}
			})
		}
	}
}
