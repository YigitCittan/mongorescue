package restore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// lateCancelRunner is a fake mongorestore that consumes its input, prints stderr
// and exits 0, but has the registry cancel the run just before Wait returns: the
// cancellation lands after the tool finished its work.
func lateCancelRunner(reg *runs.Registry, id *string, stderr string) ProcessRunner {
	return func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(stderr), func() error {
			_ = reg.Cancel(*id, runs.Cancellation{By: "alice"})
			return nil
		}, nil
	}
}

func TestCancelAfterMongorestoreFinishedKeepsTheRestore(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	reg := runs.NewRegistry()
	admin := &fakeAdmin{}
	req := models.RestoreRequest{BackupID: src.ID}
	var id string
	engine := NewEngine(store, "mongodb://localhost:27017", WithDatabaseAdmin(admin),
		WithRunner(lateCancelRunner(reg, &id, "1 document(s) restored successfully. 0 document(s) failed to restore.\n")))
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	id = record.ID
	run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: id})
	defer run.End()
	ctx := run.Bind(context.Background())
	got, err := engine.Execute(ctx, req, src, record)
	if err != nil || got.Status != models.RestoreStatusCompleted || got.CancelledBy != "" {
		t.Fatalf("a restore whose mongorestore exited 0 must complete despite a late cancel: %s %q, %v", got.Status, got.ErrorMessage, err)
	}
	if len(admin.dropped) != 0 {
		t.Fatalf("the finished clone was dropped: %v", admin.dropped)
	}
	if ctx.Err() == nil {
		t.Fatal("the test did not cancel the run")
	}
}

func TestCancelAfterFinishingIsRefused(t *testing.T) {
	store := storage.NewMockStorage()
	src := plainBackup(t, store, []byte("archive-bytes"))
	reg := runs.NewRegistry()
	req := models.RestoreRequest{BackupID: src.ID}
	var id string
	var cancelErr error
	exited, tried := false, false
	// The cancel arrives while the restore records its outcome: once mongorestore
	// exited, while the checksum of the remaining stream is checked.
	runner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader("1 document(s) restored successfully. 0 document(s) failed to restore.\n"), func() error { exited = true; return nil }, nil
	}
	engine := NewEngine(&afterEOFStorage{MockStorage: store, hook: func() {
		if exited && !tried {
			tried = true
			cancelErr = reg.Cancel(id, runs.Cancellation{By: "alice"})
		}
	}}, "mongodb://localhost:27017", WithRunner(runner))
	record, err := engine.Prepare(req, src)
	if err != nil {
		t.Fatal(err)
	}
	id = record.ID
	run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: id})
	defer run.End()
	got, err := engine.Execute(run.Bind(context.Background()), req, src, record)
	if err != nil || got.Status != models.RestoreStatusCompleted {
		t.Fatalf("got %s, %v", got.Status, err)
	}
	if !tried || !errors.Is(cancelErr, runs.ErrFinishing) {
		t.Fatalf("cancel after mongorestore exited 0 = %v, want ErrFinishing", cancelErr)
	}
}

// afterEOFStorage calls hook when a Retrieve stream is read again after its EOF,
// which the restore does once mongorestore exited, to hash any unread bytes.
type afterEOFStorage struct {
	*storage.MockStorage
	hook func()
}

func (s *afterEOFStorage) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.MockStorage.Retrieve(ctx, key)
	if err != nil {
		return nil, err
	}
	return &afterEOFReader{ReadCloser: rc, hook: s.hook}, nil
}

type afterEOFReader struct {
	io.ReadCloser
	hook func()
	eof  bool
}

func (r *afterEOFReader) Read(p []byte) (int, error) {
	if r.eof {
		r.hook()
	}
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		r.eof = true
	}
	return n, err
}

func TestCancelledRestoreWithFailedChecksIsCancelledNotFailed(t *testing.T) {
	for name, stderr := range map[string]string{
		"checksum mismatch": "1 document(s) restored successfully. 0 document(s) failed to restore.\n",
		"failed documents":  "1 document(s) restored successfully. 2 document(s) failed to restore.\n",
	} {
		t.Run(name, func(t *testing.T) {
			store := storage.NewMockStorage()
			src := plainBackup(t, store, []byte("archive-bytes"))
			if name == "checksum mismatch" {
				src.SHA256 = sha256Hex("other-bytes")
			}
			reg := runs.NewRegistry()
			admin := &fakeAdmin{}
			req := models.RestoreRequest{BackupID: src.ID}
			var id string
			engine := NewEngine(store, "mongodb://localhost:27017", WithDatabaseAdmin(admin), WithRunner(lateCancelRunner(reg, &id, stderr)))
			record, err := engine.Prepare(req, src)
			if err != nil {
				t.Fatal(err)
			}
			id = record.ID
			run, _ := reg.Register(runs.Meta{Kind: models.RunRestore, ID: id})
			got, err := engine.Execute(run.Bind(context.Background()), req, src, record)
			run.End()
			if !errors.Is(err, runs.ErrCancelled) || got.Status != models.RestoreStatusCancelled || got.CancelledBy != "alice" {
				t.Fatalf("got %s by %q (%q), %v; want cancelled", got.Status, got.CancelledBy, got.ErrorMessage, err)
			}
			if len(admin.dropped) != 1 || admin.dropped[0] != record.TargetDatabase || !strings.Contains(got.ErrorMessage, "was dropped") {
				t.Fatalf("dropped %v, message %q; a cancelled clone is dropped", admin.dropped, got.ErrorMessage)
			}
		})
	}
}
