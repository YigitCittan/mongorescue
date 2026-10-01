package operations_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// listerEngine is a restore engine whose archive listing is replaced by fn.
type listerEngine struct {
	*restore.Engine
	calls atomic.Int32
	fn    func(ctx context.Context, rec *models.BackupRecord) ([]models.BackupCollection, error)
}

func (l *listerEngine) ArchiveCollections(ctx context.Context, rec *models.BackupRecord) ([]models.BackupCollection, error) {
	l.calls.Add(1)
	return l.fn(ctx, rec)
}

func newPreviewService(t *testing.T, eng operations.RestoreEngine, timeout time.Duration) (*operations.Service, *store.SQLiteStore) {
	t.Helper()
	st := storetest.New(t)
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	return operations.New(operations.Config{
		Store:          st,
		Backup:         backup.NewEngine(storage.NewMockStorage(), "", backup.WithRunner(bRunner)),
		Restore:        eng,
		Runs:           manager,
		PreviewTimeout: timeout,
	}), st
}

func saveBackup(t *testing.T, st *store.SQLiteStore, rec *models.BackupRecord) {
	t.Helper()
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().UTC()
	}
	if err := st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

// TestListBackupCollectionsFromArchive lists a real archive through the restore
// engine, and serves the second request from the cache.
func TestListBackupCollectionsFromArchive(t *testing.T) {
	archive, err := os.ReadFile(filepath.Join("..", "mongotools", "testdata", "prelude", "shop.archive.gz"))
	if err != nil {
		t.Fatal(err)
	}
	mock := storage.NewMockStorage()
	if _, err = mock.Save(context.Background(), "shop/a.archive.gz", bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	real := restore.NewEngine(mock, "")
	eng := &listerEngine{Engine: real, fn: real.ArchiveCollections}
	svc, st := newPreviewService(t, eng, 0)
	saveBackup(t, st, &models.BackupRecord{ID: "bkp_a", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/a.archive.gz"})

	for i := range 2 {
		got, err := svc.ListBackupCollections(context.Background(), "bkp_a")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != operations.CollectionsFromArchive || got.Database != "shop" || got.Warning != "" || len(got.Collections) != 4 {
			t.Fatalf("call %d: %+v", i, got)
		}
		if v := got.Collections[3]; v.Name != "big_orders" || v.Type != models.CollectionTypeView || v.ViewOn != "orders" {
			t.Fatalf("view = %+v", v)
		}
		got.Collections[0].Name = "mutated" // the cache hands out copies
	}
	if n := eng.calls.Load(); n != 1 {
		t.Fatalf("archive read %d times; want 1 (cached)", n)
	}
}

func TestListBackupCollectionsFallsBackToTheRecord(t *testing.T) {
	keyErr := fmt.Errorf("%w: %s", encryption.ErrEncryptionKeyRequired, restore.KeyRequiredHint)
	var fail error
	eng := &listerEngine{Engine: restore.NewEngine(storage.NewMockStorage(), ""), fn: func(context.Context, *models.BackupRecord) ([]models.BackupCollection, error) {
		return nil, fail
	}}
	svc, st := newPreviewService(t, eng, 0)
	saveBackup(t, st, &models.BackupRecord{ID: "bkp_sel", Database: "shop", Status: models.StatusCompleted, StorageKey: "k1", Collections: []string{"orders", " ", "customers"}})
	saveBackup(t, st, &models.BackupRecord{ID: "bkp_all", Database: "shop", Status: models.StatusCompleted, StorageKey: "k2"})
	ctx := context.Background()

	// An old or damaged archive: the record's own list, marked as such.
	fail = fmt.Errorf("read archive prelude: %w", errors.New("mongotools: malformed archive prelude: secret mongodb://u:hunter2@h"))
	got, err := svc.ListBackupCollections(ctx, "bkp_sel")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != operations.CollectionsFromRecord || len(got.Collections) != 2 || got.Collections[1].Name != "customers" ||
		!strings.Contains(got.Warning, "could not be read") || strings.Contains(got.Warning, "hunter2") {
		t.Fatalf("fallback = %+v", got)
	}
	// A whole-database backup has no list of its own: an empty list with the reason.
	got, err = svc.ListBackupCollections(ctx, "bkp_all")
	if err != nil || got.Source != operations.CollectionsFromRecord || got.Collections == nil || len(got.Collections) != 0 || got.Warning == "" {
		t.Fatalf("fallback without a record list = %+v, %v", got, err)
	}

	// A missing key: the restore hint, as a warning when the record has a list and as
	// ErrKeyRequired when it has none.
	fail = keyErr
	got, err = svc.ListBackupCollections(ctx, "bkp_sel")
	if err != nil || got.Source != operations.CollectionsFromRecord || !strings.Contains(got.Warning, restore.KeyRequiredHint) {
		t.Fatalf("missing key with a record list = %+v, %v", got, err)
	}
	_, err = svc.ListBackupCollections(ctx, "bkp_all")
	if !errors.Is(err, operations.ErrKeyRequired) || !strings.Contains(err.Error(), restore.KeyRequiredHint) {
		t.Fatalf("missing key without a record list: %v; want ErrKeyRequired with the hint", err)
	}

	// Fallbacks are not cached: a key added later is used.
	fail = nil
	eng.fn = func(context.Context, *models.BackupRecord) ([]models.BackupCollection, error) {
		return []models.BackupCollection{{Name: "orders", Type: models.CollectionTypeCollection}}, nil
	}
	if got, err = svc.ListBackupCollections(ctx, "bkp_all"); err != nil || got.Source != operations.CollectionsFromArchive {
		t.Fatalf("after the key was added: %+v, %v", got, err)
	}
}

func TestListBackupCollectionsTimeout(t *testing.T) {
	eng := &listerEngine{Engine: restore.NewEngine(storage.NewMockStorage(), ""), fn: func(ctx context.Context, _ *models.BackupRecord) ([]models.BackupCollection, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}}
	svc, st := newPreviewService(t, eng, 50*time.Millisecond)
	saveBackup(t, st, &models.BackupRecord{ID: "bkp_slow", Database: "shop", Status: models.StatusCompleted, StorageKey: "k", Collections: []string{"orders"}})
	got, err := svc.ListBackupCollections(context.Background(), "bkp_slow")
	if err != nil || got.Source != operations.CollectionsFromRecord || !strings.Contains(got.Warning, "timed out") {
		t.Fatalf("slow archive = %+v, %v", got, err)
	}

	// A client that goes away gets its own error, not a fallback.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = svc.ListBackupCollections(ctx, "bkp_slow"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v; want context.Canceled", err)
	}
}

func TestListBackupCollectionsNotFound(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.ListBackupCollections(context.Background(), "bkp_missing"); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing backup: %v; want ErrNotFound", err)
	}
	if _, err := svc.ListBackupCollections(context.Background(), " "); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("blank id: %v; want ErrInvalid", err)
	}
}

// TestListBackupCollectionsInProgressNotCached checks that only completed backups
// are cached (an in-progress backup may still fail).
func TestListBackupCollectionsInProgressNotCached(t *testing.T) {
	eng := &listerEngine{Engine: restore.NewEngine(storage.NewMockStorage(), ""), fn: func(context.Context, *models.BackupRecord) ([]models.BackupCollection, error) {
		return []models.BackupCollection{{Name: "orders", Type: models.CollectionTypeCollection}}, nil
	}}
	svc, st := newPreviewService(t, eng, 0)
	saveBackup(t, st, &models.BackupRecord{ID: "bkp_run", Database: "shop", Status: models.StatusInProgress, StorageKey: "k"})
	for range 2 {
		if _, err := svc.ListBackupCollections(context.Background(), "bkp_run"); err != nil {
			t.Fatal(err)
		}
	}
	if n := eng.calls.Load(); n != 2 {
		t.Fatalf("archive read %d times; want 2 (not cached)", n)
	}
}
