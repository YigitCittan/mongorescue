package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// brokenStore refuses every upload.
type brokenStore struct{ *storage.MockStorage }

func (brokenStore) Save(context.Context, string, io.Reader) (*models.StorageObject, error) {
	return nil, errors.New("copy target unreachable")
}

// copyEngine returns an engine writing to drivers["primary"] that copies with a
// copies.Service over drivers.
func copyEngine(drivers map[string]storage.Storage) *Engine {
	resolve := func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil }
	svc := copies.New(copies.Config{Storages: resolve, SyncBackoff: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("complete archive bytes"))), strings.NewReader(""), func() error { return nil }, nil
	}
	return NewEngine(nil, "mongodb://localhost:27017", WithRunner(runner), WithStorageResolver(resolve), WithCopier(svc.CopyAll),
		WithLogger(slog.New(slog.DiscardHandler)))
}

func copyOptions(mode models.CopyMode, targets ...string) models.BackupOptions {
	opts := models.BackupOptions{Database: "shop", StorageTargetID: "primary", CopyMode: mode}
	for _, id := range targets {
		opts.Copies = append(opts.Copies, models.CopyTarget{ID: id, Name: id})
	}
	return opts
}

func TestAsyncCopiesAreLeftToTheQueue(t *testing.T) {
	drivers := map[string]storage.Storage{"primary": storage.NewMockStorage(), "copy": storage.NewMockStorage()}
	rec, err := copyEngine(drivers).Run(context.Background(), copyOptions("", "copy"))
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("run = %+v, %v", rec, err)
	}
	if rec.CopyMode != models.CopyAsync || len(rec.Copies) != 1 || rec.Copies[0].Status != models.CopyPending || rec.Copies[0].StorageKey != rec.StorageKey {
		t.Fatalf("copies = %+v", rec.Copies)
	}
	if _, err = drivers["copy"].Stat(context.Background(), rec.StorageKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("an async copy must not be made by the backup itself: %v", err)
	}
}

func TestSyncCopiesCompleteTheBackup(t *testing.T) {
	drivers := map[string]storage.Storage{"primary": storage.NewMockStorage(), "copy": storage.NewMockStorage()}
	rec, err := copyEngine(drivers).Run(context.Background(), copyOptions(models.CopySync, "copy"))
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("run = %+v, %v", rec, err)
	}
	if c := rec.Copies[0]; c.Status != models.CopyDone || !c.SHA256OK || c.CopiedAt == nil {
		t.Fatalf("copy = %+v", c)
	}
	if _, err = drivers["copy"].Stat(context.Background(), rec.StorageKey); err != nil {
		t.Fatalf("the copy was not stored: %v", err)
	}
}

func TestFailedSyncCopyFailsTheBackupAndRemovesEveryArchive(t *testing.T) {
	ctx := context.Background()
	drivers := map[string]storage.Storage{"primary": storage.NewMockStorage(), "good": storage.NewMockStorage(),
		"bad": brokenStore{storage.NewMockStorage()}}
	rec, err := copyEngine(drivers).Run(ctx, copyOptions(models.CopySync, "good", "bad"))
	if !errors.Is(err, ErrCopyFailed) || rec.Status != models.StatusFailed {
		t.Fatalf("run = %+v, %v; want ErrCopyFailed", rec, err)
	}
	for _, id := range []string{"primary", "good"} {
		if _, statErr := drivers[id].Stat(ctx, rec.StorageKey); !errors.Is(statErr, storage.ErrNotFound) {
			t.Fatalf("the archive on %s must be deleted: %v", id, statErr)
		}
	}
	if rec.Copies[0].Status != models.CopyPurged || rec.Copies[1].Status != models.CopyFailed || rec.Copies[1].Error == "" {
		t.Fatalf("copies = %+v", rec.Copies)
	}
}

func TestFailedSyncCopyLeavesALockedCopyToThePurge(t *testing.T) {
	until := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	locked := &lockedStore{MockStorage: storage.NewMockStorage(), until: until}
	drivers := map[string]storage.Storage{"primary": storage.NewMockStorage(), "locked": locked, "bad": brokenStore{storage.NewMockStorage()}}
	rec, err := copyEngine(drivers).Run(context.Background(), copyOptions(models.CopySync, "locked", "bad"))
	if !errors.Is(err, ErrCopyFailed) {
		t.Fatalf("run = %v", err)
	}
	c := rec.Copies[0]
	if c.Status != models.CopyDone || c.VersionID != "v1" || c.RetainUntil == nil || !c.RetainUntil.Equal(until) || !rec.ArchiveCleanupPending {
		t.Fatalf("locked copy = %+v, cleanup pending %v", c, rec.ArchiveCleanupPending)
	}
	if !rec.LockedAt(time.Now()) {
		t.Fatal("the record must stay locked while its copy is")
	}
}

func TestFailedBackupAbandonsItsCopies(t *testing.T) {
	drivers := map[string]storage.Storage{"primary": storage.NewMockStorage(), "copy": storage.NewMockStorage()}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("partial"))), strings.NewReader(""), func() error { return errors.New("exit status 1") }, nil
	}
	e := copyEngine(drivers)
	e.runner = runner
	rec, err := e.Run(context.Background(), copyOptions("", "copy"))
	if err == nil || rec.Copies[0].Status != models.CopyFailed || rec.Copies[0].Error != models.ErrNotCopied || rec.HoldsTarget("copy") {
		t.Fatalf("run = %+v, %v", rec.Copies, err)
	}
}
