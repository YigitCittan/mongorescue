package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestInterruptedBackupSettlesItsCopies simulates a restart after a crash during
// a synchronous backup with two copy targets: one copy was made, the other was
// not. Recovery fails the backup, the copy never made keeps no target in use, and
// the purge deletes the copy that was made.
func TestInterruptedBackupSettlesItsCopies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	const key = "d/2026/10/bkp_crash.archive.gz"
	made := storage.NewMockStorage()
	if _, err := made.Save(ctx, key, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	rec := &models.BackupRecord{ID: "bkp_crash", Database: "d", Status: models.StatusInProgress, StorageTargetID: "tgt_p",
		StorageKey: key, StartedAt: time.Now().Add(-time.Hour)}
	rec.PlanCopies([]models.CopyTarget{{ID: "tgt_made"}, {ID: "tgt_never"}}, models.CopySync)
	rec.Copies[0].Status, rec.Copies[0].Attempts = models.CopyDone, 1

	// The process that crashed left the record in progress.
	box := storetest.NewBox(t)
	crashed, err := store.OpenSQLite(ctx, path, slog.New(slog.DiscardHandler), store.WithSecretBox(box))
	if err != nil {
		t.Fatal(err)
	}
	if err = crashed.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err = crashed.Close(); err != nil {
		t.Fatal(err)
	}

	// The restart opens the same database and recovers.
	first := storetest.OpenWithBox(t, path, box)
	a := &App{metaStore: first, logger: slog.New(slog.DiscardHandler)}
	a.failInterruptedRuns(ctx)
	got, _ := first.GetBackupRecord(ctx, "bkp_crash")
	if got.Status != models.StatusFailed || !got.ArchiveCleanupPending {
		t.Fatalf("after recovery: status %s, cleanup pending %v", got.Status, got.ArchiveCleanupPending)
	}
	if c := got.Copies[1]; c.Status != models.CopyFailed || c.MayExist() || got.HoldsTarget("tgt_never") {
		t.Fatalf("the copy never made = %+v", c)
	}
	drivers := map[string]storage.Storage{"tgt_p": storage.NewMockStorage(), "tgt_made": made}
	resolve := func(_ context.Context, id string) (storage.Storage, error) {
		if d, ok := drivers[id]; ok {
			return d, nil
		}
		return nil, errors.New("unknown target")
	}
	if _, err := scheduler.PurgeDeleted(ctx, time.Now(), time.Hour, first, resolve, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := made.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the copy made before the crash must be purged: %v", err)
	}
	got, _ = first.GetBackupRecord(ctx, "bkp_crash")
	if got.ArchiveCleanupPending || got.Copies[0].Status != models.CopyPurged || got.HoldsTarget("tgt_made") {
		t.Fatalf("after the purge: cleanup pending %v, copies %+v", got.ArchiveCleanupPending, got.Copies)
	}
}
