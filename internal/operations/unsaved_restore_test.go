package operations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// fullRestoreStore fails to save final (not in-progress) restore records, like
// the metadata database on a full data disk.
type fullRestoreStore struct {
	*store.SQLiteStore
	full atomic.Bool
}

func (f *fullRestoreStore) SaveRestoreRecord(ctx context.Context, rec *models.RestoreRecord) error {
	if f.full.Load() && rec.Status != models.RestoreStatusInProgress {
		return fmt.Errorf("store: save restore record %s: %w", rec.ID, errors.New("database or disk is full (13)"))
	}
	return f.SQLiteStore.SaveRestoreRecord(ctx, rec)
}

// A restore whose final record cannot be saved is never reported as succeeded:
// restore.failed is published instead, its safe clone is dropped like the clone
// of any failed restore, the disk-full alert fires, and the failed record is saved
// once writes work again.
func TestUnsavedRestoreRecordFailsTheRestore(t *testing.T) {
	st := &fullRestoreStore{SQLiteStore: storetest.New(t)}
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	pub := &recordingPublisher{}
	var free atomic.Uint64
	free.Store(1 << 40)
	guard := diskguard.New(diskguard.Config{Dir: t.TempDir(), Publisher: pub, SaveRetryDelays: []time.Duration{time.Millisecond},
		Free: func(string) (uint64, error) { return free.Load(), nil }})
	dropper := &recordingDropper{}
	svc := operations.New(operations.Config{
		Store:       st,
		Backup:      backup.NewEngine(storage.NewMockStorage(), ""),
		Restore:     &completingEngine{prep: restore.NewEngine(nil, "")},
		Runs:        manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "prod", URI: "mongodb://u:secret@db.internal:27017"}},
		Publisher:   pub,
		Dropper:     dropper,
		DiskGuard:   guard,
	})
	src := &models.BackupRecord{ID: "bkp_shop", Database: "shop", ConnectionID: "conn_a", Status: models.StatusCompleted,
		StartedAt: time.Now().UTC(), StorageKey: "shop/bkp_shop.archive.gz", SizeBytes: 10, SHA256: "abc"}
	if err := st.SaveBackupRecord(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	st.full.Store(true)
	rec, err := svc.StartRestore(admin(), models.RestoreRequest{BackupID: src.ID})
	if err != nil {
		t.Fatal(err)
	}
	var got []events.Event
	deadline := time.Now().Add(5 * time.Second)
	for {
		pub.mu.Lock()
		got = append(got[:0], pub.events...)
		pub.mu.Unlock()
		if len(got) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	var types []events.EventType
	for _, e := range got {
		types = append(types, e.Type)
		if e.Type == events.RestoreSucceeded {
			t.Fatalf("restore.succeeded published for an unsaved restore: %+v", e)
		}
	}
	if !containsEvent(types, events.RestoreFailed) || !containsEvent(types, events.SystemDiskFull) {
		t.Fatalf("events %v; want restore.failed and system.disk_full", types)
	}
	dropper.mu.Lock()
	dropped := append([]string(nil), dropper.dropped...)
	dropper.mu.Unlock()
	if len(dropped) != 1 || dropped[0] != rec.TargetDatabase {
		t.Fatalf("dropped %v; want the clone %s", dropped, rec.TargetDatabase)
	}

	waitIdle(t, manager)
	st.full.Store(false)
	_ = guard.Check()
	guard.RetryDeferred(context.Background())
	stored, err := st.GetRestoreRecord(context.Background(), rec.ID)
	if err != nil || stored.Status != models.RestoreStatusFailed || !strings.HasPrefix(stored.ErrorMessage, models.ErrRestoreRecordNotSaved) ||
		!strings.Contains(stored.ErrorMessage, "was dropped") {
		t.Fatalf("settled record %+v, %v", stored, err)
	}

	// While the disk is full new restores are refused with 503 semantics.
	manager.SetAdmission(guard.Admit)
	free.Store(0)
	guard.Observe(context.Background(), errors.New("database or disk is full"))
	if _, err := svc.StartRestore(admin(), models.RestoreRequest{BackupID: src.ID}); !errors.Is(err, operations.ErrUnavailable) {
		t.Fatalf("restore while full = %v; want ErrUnavailable", err)
	}
}

func containsEvent(list []events.EventType, t events.EventType) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}
