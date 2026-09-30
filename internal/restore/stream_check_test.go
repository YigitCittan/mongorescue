package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestRestoreStreamingChecksumMismatchFailsWithoutVerify covers a safe-clone restore
// (no verification pass by default) of an artifact that changed at rest: mongorestore
// accepts the bytes, but the restore must fail with ErrChecksumMismatch.
func TestRestoreStreamingChecksumMismatchFailsWithoutVerify(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = sha256Hex("archive-bytez") // one flipped byte at rest

	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if !runner.called || record.Verified {
		t.Fatalf("safe clone without verify must stream once: called=%v verified=%v", runner.called, record.Verified)
	}
	if record.Status != models.RestoreStatusFailed || !strings.Contains(record.ErrorMessage, "must not be trusted") {
		t.Fatalf("unexpected record: %s %q", record.Status, record.ErrorMessage)
	}
}

// TestRestoreStreamingChecksumHashesUnreadTail makes mongorestore stop reading before
// EOF: the rest of the artifact is still hashed, so a matching artifact succeeds and a
// damaged tail fails.
func TestRestoreStreamingChecksumHashesUnreadTail(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 10_000)
	partial := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.CopyN(io.Discard, stdin, 1000)
		return strings.NewReader(""), func() error { return nil }, nil
	}

	store := storage.NewMockStorage()
	src := plainBackup(t, store, payload)
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(partial))
	if _, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); err != nil {
		t.Fatalf("matching artifact: %v", err)
	}

	damaged := slices.Clone(payload)
	damaged[len(damaged)-1] ^= 0xff
	src.SHA256 = sha256Hex(damaged)
	if _, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("damaged tail: expected ErrChecksumMismatch, got %v", err)
	}
}

// TestRestoreLegacyRecordWithoutChecksumStillRestores keeps records from releases
// that did not record a SHA-256 restorable.
func TestRestoreLegacyRecordWithoutChecksumStillRestores(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = ""
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner((&capturingRunner{}).run))
	if record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src); err != nil || record.Status != models.RestoreStatusCompleted {
		t.Fatalf("legacy record: %v", err)
	}
}

// TestRestoreFailsWhenDocumentsFailed covers mongorestore exiting 0 although it could
// not insert some documents (duplicate keys in a target that already held data).
func TestRestoreFailsWhenDocumentsFailed(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	summary := "continuing through error: E11000 duplicate key error collection: db.orders index: _id_\n" +
		"2026-09-30T12:00:00.000+0000\t8 document(s) restored successfully. 2 document(s) failed to restore.\n"
	runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(summary), func() error { return nil }, nil
	}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner))
	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if !errors.Is(err, ErrDocumentsFailed) {
		t.Fatalf("expected ErrDocumentsFailed, got %v", err)
	}
	if record.Status != models.RestoreStatusFailed ||
		!strings.Contains(record.ErrorMessage, "2 document(s) failed to restore") ||
		!strings.Contains(record.ErrorMessage, "E11000") {
		t.Fatalf("unexpected record: %s %q", record.Status, record.ErrorMessage)
	}
}

func TestFailedDocuments(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   int64
		wantOK bool
	}{
		{"10 document(s) restored successfully. 0 document(s) failed to restore.", 0, true},
		{"x\n3 document(s) restored successfully. 7 document(s) failed to restore.\n", 7, true},
		{"no summary", 0, false},
	} {
		got, ok := failedDocuments(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("failedDocuments(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestRestoreBypassesValidationOnlyWithThePrivilege(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check BypassCheck
		want  bool
	}{
		{"no check", nil, false},
		{"privilege held", func(context.Context, string, string) (bool, error) { return true, nil }, true},
		{"privilege missing", func(context.Context, string, string) (bool, error) { return false, nil }, false},
		{"check failed", func(context.Context, string, string) (bool, error) { return true, errors.New("unreachable") }, false},
	} {
		store := storage.NewMockStorage()
		src := plainBackup(t, store, []byte("archive-bytes"))
		runner := &capturingRunner{}
		var checkedDB string
		opts := []Option{WithRunner(runner.run)}
		if tc.check != nil {
			check := tc.check
			opts = append(opts, WithValidationBypassCheck(func(ctx context.Context, uri, db string) (bool, error) {
				checkedDB = db
				return check(ctx, uri, db)
			}))
		}
		engine := NewEngine(store, "mongodb://localhost:27017", opts...)
		record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := slices.Contains(runner.args, "--bypassDocumentValidation"); got != tc.want {
			t.Errorf("%s: --bypassDocumentValidation passed = %v; want %v", tc.name, got, tc.want)
		}
		if tc.check != nil && checkedDB != record.TargetDatabase {
			t.Errorf("%s: privilege checked on %q; want the target %q", tc.name, checkedDB, record.TargetDatabase)
		}
	}
}
