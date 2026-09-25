package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// inPlaceRequest builds a confirmed restore into the source namespace.
func inPlaceRequest(backupID string) models.RestoreRequest {
	inPlace := false
	return models.RestoreRequest{BackupID: backupID, SafeClone: &inPlace, ConfirmInPlace: true}
}

func sha256Hex[T string | []byte](data T) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// retrieveCountingStorage counts Retrieve calls: two means a verification pass ran.
type retrieveCountingStorage struct {
	*storage.MockStorage
	retrieves atomic.Int32
}

func (c *retrieveCountingStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	c.retrieves.Add(1)
	return c.MockStorage.Retrieve(ctx, key)
}

func plainBackup(t *testing.T, store storage.Storage, payload []byte) *models.BackupRecord {
	t.Helper()
	key := "db/2026/09/bkp_db_20260924_120000.archive.gz"
	if _, err := store.Save(context.Background(), key, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	return &models.BackupRecord{ID: "bkp_db_20260924_120000", Database: "db", StorageKey: key, SHA256: sha256Hex(payload)}
}

func TestRestoreVerifyChecksumMismatchNeverStartsMongorestore(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = sha256Hex("different-bytes") // bit rot / tampering at rest

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if runner.called {
		t.Fatal("mongorestore must never start when the checksum does not match")
	}
	if record.Status != models.RestoreStatusFailed || record.Verified {
		t.Fatalf("unexpected record: status=%s verified=%v", record.Status, record.Verified)
	}
}

func TestRestoreTimeoutAbortsAndWarnsAboutPartialData(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	hanging := func(ctx context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		<-ctx.Done() // mongorestore stuck until killed
		return strings.NewReader(""), func() error { return errors.New("signal: terminated") }, nil
	}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(hanging), WithTimeout(100*time.Millisecond))

	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if record.Status != models.RestoreStatusFailed ||
		!strings.Contains(record.ErrorMessage, "partial data may have been applied to target namespace db.*") {
		t.Fatalf("unexpected record: %s %q", record.Status, record.ErrorMessage)
	}
}

func TestRestoreEngineRejectsUnconfirmedInPlace(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))

	inPlace := false
	for _, req := range []models.RestoreRequest{
		{BackupID: src.ID, SafeClone: &inPlace},
		{BackupID: src.ID, TargetDatabase: src.Database},
	} {
		record, err := engine.Run(context.Background(), req, src)
		if !errors.Is(err, models.ErrInPlaceNotConfirmed) || record != nil {
			t.Fatalf("expected ErrInPlaceNotConfirmed with no record, got %v / %+v", err, record)
		}
	}
	if runner.called {
		t.Fatal("mongorestore must never run for an unconfirmed in-place restore")
	}

	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if err != nil || record.TargetDatabase != src.Database {
		t.Fatalf("confirmed in-place restore: target=%v err=%v", record, err)
	}
}

func TestRestoreVerifyWithoutChecksum(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = ""

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
	verify := true
	_, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID, Verify: &verify}, src)
	if !errors.Is(err, ErrChecksumUnavailable) || runner.called {
		t.Fatalf("expected ErrChecksumUnavailable before mongorestore, got %v (called=%v)", err, runner.called)
	}
}

func TestRestoreVerifyPolicyMatrix(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		policy    models.VerifyPolicy
		safeClone bool
		request   *bool
		want      bool
	}{
		{models.VerifyAuto, false, nil, true},
		{models.VerifyAuto, true, nil, false},
		{models.VerifyAuto, false, &no, false},
		{models.VerifyAuto, true, &yes, true},
		{models.VerifyAlways, true, nil, true},
		{models.VerifyAlways, false, nil, true},
		{models.VerifyAlways, false, &no, false},
		{models.VerifyNever, false, nil, false},
		{models.VerifyNever, true, nil, false},
		{models.VerifyNever, true, &yes, true},
	}
	payload := []byte("archive-bytes")
	for _, tc := range cases {
		req := "unset"
		if tc.request != nil {
			req = fmt.Sprint(*tc.request)
		}
		t.Run(fmt.Sprintf("%s/safe_clone=%v/verify=%s", tc.policy, tc.safeClone, req), func(t *testing.T) {
			store := &retrieveCountingStorage{MockStorage: storage.NewMockStorage()}
			src := plainBackup(t, store, payload)
			runner := &capturingRunner{}
			engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithVerifyPolicy(tc.policy))

			record, err := engine.Run(context.Background(),
				models.RestoreRequest{BackupID: src.ID, SafeClone: &tc.safeClone, ConfirmInPlace: !tc.safeClone, Verify: tc.request}, src)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if record.Verified != tc.want {
				t.Fatalf("verified = %v, want %v", record.Verified, tc.want)
			}
			wantRetrieves := int32(1)
			if tc.want {
				wantRetrieves = 2
			}
			if got := store.retrieves.Load(); got != wantRetrieves {
				t.Fatalf("storage retrieves = %d, want %d", got, wantRetrieves)
			}
			if !bytes.Equal(runner.stdin, payload) {
				t.Fatal("mongorestore did not receive the full archive after verification")
			}
		})
	}
}

// missingFinalChunk encrypts payload and drops the last (short) age chunk, leaving
// a stream whose every present chunk still authenticates.
func missingFinalChunk(t *testing.T, enc *encryption.Encryptor) []byte {
	t.Helper()
	const chunk, tail = 64 * 1024, 100
	full := sealPayload(t, enc, bytes.Repeat([]byte("k"), 2*chunk+tail))
	return full[:len(full)-(tail+16)]
}

func TestRestoreVerifyAuthenticatesEncryptedStream(t *testing.T) {
	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	truncated := missingFinalChunk(t, enc)

	store := storage.NewMockStorage()
	key := "db/2026/09/bkp.archive.gz.age"
	_, _ = store.Save(context.Background(), key, bytes.NewReader(truncated))
	// The checksum matches the (truncated) stored bytes: only age authentication of
	// the final chunk can reveal the damage.
	src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, SHA256: sha256Hex(truncated), Encrypted: true}

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})), WithVerifyPolicy(models.VerifyAlways))
	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, encryption.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed from verification, got %v", err)
	}
	if runner.called {
		t.Fatal("mongorestore must not start when verification fails")
	}
	if !strings.Contains(record.ErrorMessage, "target untouched") {
		t.Fatalf("unexpected error message: %q", record.ErrorMessage)
	}
}

func TestRestoreWithoutVerifyReportsPartialApply(t *testing.T) {
	id, r := newKeyPair(t)
	enc, _ := encryption.NewX25519Encryptor([]string{r})
	truncated := missingFinalChunk(t, enc)

	store := storage.NewMockStorage()
	key := "db/2026/09/bkp.archive.age"
	_, _ = store.Save(context.Background(), key, bytes.NewReader(truncated))
	src := &models.BackupRecord{ID: "bkp", Database: "db", StorageKey: key, SHA256: sha256Hex(truncated), Encrypted: true}

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run),
		WithDecryptor(mustDecryptor(t, encryption.DecryptorConfig{Identity: id})))
	no := false
	record, err := engine.Run(context.Background(),
		models.RestoreRequest{BackupID: src.ID, TargetDatabase: "db_restored", SafeClone: &no, ConfirmInPlace: true, Verify: &no}, src)
	if !errors.Is(err, encryption.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
	if !runner.called {
		t.Fatal("without verification mongorestore should have started")
	}
	want := "partial data may have been applied to target namespace db_restored.*"
	if !strings.Contains(record.ErrorMessage, want) {
		t.Fatalf("error message %q must contain %q", record.ErrorMessage, want)
	}
}
