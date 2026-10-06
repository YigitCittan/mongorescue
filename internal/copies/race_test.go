package copies_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// blockingStorage holds every Save until release is closed, and reports on
// started that one began.
type blockingStorage struct {
	*storage.MockStorage
	started chan struct{}
	release chan struct{}
}

func (b *blockingStorage) Save(ctx context.Context, k string, r io.Reader) (*models.StorageObject, error) {
	close(b.started)
	<-b.release
	return b.MockStorage.Save(ctx, k, r)
}

func newBlocking() *blockingStorage {
	return &blockingStorage{MockStorage: storage.NewMockStorage(), started: make(chan struct{}), release: make(chan struct{})}
}

// TestPurgeWaitsForAnInFlightCopy proves that a backup deleted and purged while
// its copy uploads never leaves the copy behind: the purge waits for the upload,
// which records the copy on the deleted backup, and then deletes it.
func TestPurgeWaitsForAnInFlightCopy(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	if err := st.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := newBlocking()
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	resolve := func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil }
	svc := copies.New(copies.Config{Store: st, Storages: resolve, Logger: slog.New(slog.DiscardHandler)})

	copied := make(chan error, 1)
	go func() { copied <- svc.RunDue(ctx) }()
	<-dst.started

	// Deleted while it uploads, with a grace period that has already passed.
	past := time.Now().Add(-48 * time.Hour)
	if _, err := st.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		r.MarkDeleted(models.SoftDelete{At: past, PurgeAfter: past.Add(time.Hour), By: "admin"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	purged := make(chan []string, 1)
	go func() {
		ids, _ := scheduler.PurgeDeleted(ctx, time.Now(), time.Hour, st, resolve, nil, nil)
		purged <- ids
	}()
	select {
	case ids := <-purged:
		t.Fatalf("the purge ran during the upload: %v", ids)
	case <-time.After(200 * time.Millisecond):
	}
	close(dst.release)
	if err := <-copied; err != nil {
		t.Fatalf("copy: %v", err)
	}
	if ids := <-purged; len(ids) != 1 {
		t.Fatalf("purged %v", ids)
	}
	if _, err := dst.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the copy made during the deletion must be purged: %v", err)
	}
	rec, _ := st.GetBackupRecord(ctx, "bkp_1")
	if rec.Status != models.StatusPurged || rec.Copies[0].Status != models.CopyPurged {
		t.Fatalf("record = %s, copy %+v", rec.Status, rec.Copies[0])
	}
}

// TestCopyOfARemovedBackupIsDeleted proves that a copy whose backup record was
// removed while it uploaded is deleted again instead of being left untracked.
func TestCopyOfARemovedBackupIsDeleted(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	if err := st.SaveBackupRecord(ctx, record("copy")); err != nil {
		t.Fatal(err)
	}
	dst := newBlocking()
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	svc := copies.New(copies.Config{Store: st, Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		Logger: slog.New(slog.DiscardHandler)})
	copied := make(chan error, 1)
	go func() { copied <- svc.RunDue(ctx) }()
	<-dst.started
	if err := st.DeleteBackupRecord(ctx, "bkp_1"); err != nil {
		t.Fatal(err)
	}
	close(dst.release)
	if err := <-copied; err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("an untracked copy was left: %v", err)
	}
}
