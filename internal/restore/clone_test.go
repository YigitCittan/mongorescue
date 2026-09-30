package restore

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// fakeAdmin records the databases the engine checks and drops.
type fakeAdmin struct {
	mu      sync.Mutex
	exists  map[string]bool
	dropped []string
	dropErr error
}

func (f *fakeAdmin) DatabaseExists(_ context.Context, _, db string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[db], nil
}

func (f *fakeAdmin) DropDatabase(_ context.Context, _, db string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, db)
	return f.dropErr
}

func TestCloneWithChecksumMismatchIsDropped(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = sha256Hex("archive-bytez")
	admin := &fakeAdmin{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner((&capturingRunner{}).run), WithDatabaseAdmin(admin))

	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if len(admin.dropped) != 1 || admin.dropped[0] != record.TargetDatabase {
		t.Fatalf("dropped %v; want the clone %s", admin.dropped, record.TargetDatabase)
	}
	if !strings.Contains(record.ErrorMessage, "was dropped") {
		t.Fatalf("the record must say the clone was dropped: %q", record.ErrorMessage)
	}

	admin.dropped, admin.dropErr = nil, errors.New("not authorized")
	record, _ = engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !strings.Contains(record.ErrorMessage, "drop it manually") {
		t.Fatalf("a failed drop must be reported: %q", record.ErrorMessage)
	}
}

func TestCloneWithFailedDocumentsIsKept(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader("1 document(s) restored successfully. 1 document(s) failed to restore.\n"), func() error { return nil }, nil
	}
	admin := &fakeAdmin{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner), WithDatabaseAdmin(admin))
	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if !errors.Is(err, ErrDocumentsFailed) || record.Status != models.RestoreStatusFailed {
		t.Fatalf("expected a failed record with ErrDocumentsFailed, got %v", err)
	}
	if len(admin.dropped) != 0 {
		t.Fatalf("a clone with failed documents is kept for inspection, dropped %v", admin.dropped)
	}
}

func TestExistingCloneTargetIsRefused(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	runner := &capturingRunner{}
	admin := &fakeAdmin{exists: map[string]bool{}}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithDatabaseAdmin(admin))

	req := models.RestoreRequest{BackupID: src.ID}
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	admin.exists[record.TargetDatabase] = true
	record, err = engine.Execute(context.Background(), req, src, record)
	if !errors.Is(err, ErrCloneExists) || runner.called || len(admin.dropped) != 0 {
		t.Fatalf("expected ErrCloneExists before mongorestore, got %v (called=%v, dropped=%v)", err, runner.called, admin.dropped)
	}
	if record.Status != models.RestoreStatusFailed {
		t.Fatalf("status = %s", record.Status)
	}
}

func TestInPlaceIsNeverDroppedAndAlwaysVerified(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = sha256Hex("archive-bytez")
	runner := &capturingRunner{}
	admin := &fakeAdmin{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run), WithDatabaseAdmin(admin),
		WithVerifyPolicy(models.VerifyNever))
	no := false
	req := inPlaceRequest(src.ID)
	req.Verify, req.DropTarget = &no, true
	record, err := engine.Run(context.Background(), req, src)
	if !errors.Is(err, ErrChecksumMismatch) || runner.called {
		t.Fatalf("an in-place restore must be verified first: err=%v called=%v", err, runner.called)
	}
	if len(admin.dropped) != 0 || !strings.Contains(record.ErrorMessage, "target untouched") {
		t.Fatalf("in-place target must be untouched: dropped=%v message=%q", admin.dropped, record.ErrorMessage)
	}
}

func TestInPlaceWithoutChecksumWarns(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	src.SHA256 = ""
	runner := &capturingRunner{}
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner(runner.run))
	record, err := engine.Run(context.Background(), inPlaceRequest(src.ID), src)
	if err != nil || !runner.called || record.Verified {
		t.Fatalf("legacy in-place restore: err=%v called=%v verified=%v", err, runner.called, record.Verified)
	}
	if !strings.Contains(record.Warning, "no checksum") {
		t.Fatalf("warning = %q", record.Warning)
	}
}

func TestMissingDocumentSummaryIsRecorded(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	engine := NewEngine(store, "mongodb://localhost:27017", WithRunner((&capturingRunner{}).run))
	record, err := engine.Run(context.Background(), models.RestoreRequest{BackupID: src.ID}, src)
	if err != nil || record.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(record.Warning, "document counts unavailable") {
		t.Fatalf("warning = %q", record.Warning)
	}
}

func TestStderrTailBoundsLines(t *testing.T) {
	long := "E11000 duplicate key error dup key: { email: \"" + strings.Repeat("x", 1000) + "\" }"
	got := stderrTail("start\n" + long + "\n")
	if len(got) > maxStderrLine+20 || !strings.HasSuffix(got, "...") {
		t.Fatalf("stderrTail kept %d bytes: %q", len(got), got)
	}
}
