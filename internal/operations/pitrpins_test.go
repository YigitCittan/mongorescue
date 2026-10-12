package operations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// blockingPITR is fakePITR whose restores wait for release to be closed.
type blockingPITR struct {
	*fakePITR
	release chan struct{}
}

func (f *blockingPITR) ExecutePITR(ctx context.Context, req models.RestoreRequest, run restore.PITRRun, rec *models.RestoreRecord) (*models.RestoreRecord, error) {
	<-f.release
	return f.fakePITR.ExecutePITR(ctx, req, run, rec)
}

// pinnedPITRService is pitrService with its own pin set, restores that wait for
// fake.release, and the base lister bases (nil: the store's), which sees the
// store and the call number.
func pinnedPITRService(t *testing.T, bases func(st *store.SQLiteStore, call int) ([]*models.BackupRecord, error),
	saved ...*models.BackupRecord) (*operations.Service, *blockingPITR, *pitr.PinSet) {
	svc, fake, pins, _ := pinnedPITRServiceRuns(t, bases, saved...)
	return svc, fake, pins
}

// pinnedPITRServiceRuns is pinnedPITRService that also returns its run manager.
func pinnedPITRServiceRuns(t *testing.T, bases func(st *store.SQLiteStore, call int) ([]*models.BackupRecord, error),
	saved ...*models.BackupRecord) (*operations.Service, *blockingPITR, *pitr.PinSet, *runs.Manager) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	seedPITRStream(t, st, "conn_a")
	for _, b := range saved {
		if err := st.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if bases == nil {
		bases = func(st *store.SQLiteStore, _ int) ([]*models.BackupRecord, error) {
			return st.ListBaseBackups(context.Background(), "str_a")
		}
	}
	var mu sync.Mutex
	calls := 0
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	mock := storage.NewMockStorage()
	fake := &blockingPITR{fakePITR: &fakePITR{keys: true, done: make(chan struct{})}, release: make(chan struct{})}
	pins := pitr.NewPinSet(0, nil)
	svc := operations.New(operations.Config{
		Store:       st,
		Backup:      backup.NewEngine(mock, ""),
		Restore:     restore.NewEngine(mock, ""),
		Runs:        manager,
		Connections: fakeConnections{"conn_a": {ID: "conn_a", Name: "rs", URI: "mongodb://u:pw@db.internal/?replicaSet=rs0"}},
		PITR:        st,
		PITRRestore: fake,
		PITRBases: func(context.Context, string) ([]*models.BackupRecord, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			return bases(st, n)
		},
		PITRPins: pins,
	})
	return svc, fake, pins, manager
}

// awaitUnpinned waits until pins holds no pin.
func awaitUnpinned(t *testing.T, pins *pitr.PinSet) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pins.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d pin(s) left after the restore", pins.Len())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPITRRestorePinsItsPlanUntilItEnds(t *testing.T) {
	svc, fake, pins := pinnedPITRService(t, nil)
	if _, err := svc.StartRestore(admin(), pitrAt(125, "shop")); err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	snap := pins.Snapshot()
	if !snap.Base("b1") || !snap.Chunk("k110") || !snap.Chunk("k120") || !snap.Chunk("k130") {
		t.Fatalf("the running restore did not pin its plan: %+v", snap)
	}
	close(fake.release)
	<-fake.done
	awaitUnpinned(t, pins)
}

func TestChainTestPinsItsPlanUntilItEnds(t *testing.T) {
	svc, fake, pins := pinnedPITRService(t, nil, chainTestBase(7))
	if _, err := svc.StartChainTest(admin(), "str_a"); err != nil {
		t.Fatalf("StartChainTest: %v", err)
	}
	if snap := pins.Snapshot(); !snap.Base("b1") || !snap.Chunk("k110") || snap.Base("b2") {
		t.Fatalf("the running chain test did not pin B1 and its chunks: %+v", snap)
	}
	close(fake.release)
	<-fake.done
	awaitUnpinned(t, pins)
}

func TestPITRRestorePlansAgainWhenRetentionDeletedItsBase(t *testing.T) {
	// Base b2 is listed when the restore plans, then deleted by retention before
	// the pin: the check fails and the restore is planned again from b1.
	svc, fake, pins := pinnedPITRService(t, func(st *store.SQLiteStore, call int) ([]*models.BackupRecord, error) {
		list, err := st.ListBaseBackups(context.Background(), "str_a")
		if call == 1 {
			list = append(list, chainTestBase(7))
		}
		return list, err
	})
	rec, err := svc.StartRestore(admin(), pitrAt(125, "shop"))
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if rec.PITR.BaseID != "b1" {
		t.Fatalf("restore planned from %s, want b1", rec.PITR.BaseID)
	}
	if snap := pins.Snapshot(); snap.Base("b2") || !snap.Base("b1") {
		t.Fatalf("pins %+v", snap)
	}
	close(fake.release)
	<-fake.done
	awaitUnpinned(t, pins)
}

func TestPITRRestoreFailsWhenItsPlanKeepsChanging(t *testing.T) {
	// Every plan picks b2, which is gone by the time of the check.
	svc, fake, pins := pinnedPITRService(t, func(st *store.SQLiteStore, call int) ([]*models.BackupRecord, error) {
		list, err := st.ListBaseBackups(context.Background(), "str_a")
		if call%2 == 1 {
			list = append(list, chainTestBase(7))
		}
		return list, err
	})
	_, err := svc.StartRestore(admin(), pitrAt(125, "shop"))
	if !errors.Is(err, operations.ErrPITRNotRestorable) || !errors.Is(err, pitr.ErrPlanStale) {
		t.Fatalf("StartRestore = %v, want ErrPITRNotRestorable (stale plan)", err)
	}
	if pins.Len() != 0 {
		t.Fatalf("%d pin(s) left by a restore that did not start", pins.Len())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.runs) != 0 {
		t.Fatal("a restore bound to fail was started")
	}
}

func TestPITRRestoreReleasesItsPinWhenItDoesNotStart(t *testing.T) {
	svc, _, pins, manager := pinnedPITRServiceRuns(t, nil)
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartRestore(admin(), pitrAt(125, "shop")); err == nil {
		t.Fatal("a restore started on a stopped run manager")
	}
	if pins.Len() != 0 {
		t.Fatalf("%d pin(s) left by a restore that did not start", pins.Len())
	}
}
