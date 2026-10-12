package ha

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// newFloorManager returns a manager whose epoch floor (the snapshots on the target,
// its own state) is floor, or fails with floorErr.
func newFloorManager(t *testing.T, st *memStore, clock *fakeClock, id string, floor uint64, floorErr error) *Manager {
	t.Helper()
	m, err := New(Config{Store: st, Cluster: "prod", InstanceID: id, KeyID: "key1", TTL: testTTL, Renew: testRenew,
		Now: clock.Now, Wall: clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		EpochFloor: func(context.Context) (uint64, error) { return floor, floorErr }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestDeletedLeaseDoesNotRestartTheEpochs covers a lease deleted by hand while
// snapshots of epoch 9 exist: a freshly started instance acquires epoch 10, so its
// snapshots sort above the old ones.
func TestDeletedLeaseDoesNotRestartTheEpochs(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	m := newFloorManager(t, st, clock, "fresh", 9, nil)
	m.Step(ctx)
	if m.Epoch() != 10 || !m.AcquiredFresh() {
		t.Fatalf("epoch %d (fresh %v) after the lease was deleted; want 10, fresh", m.Epoch(), m.AcquiredFresh())
	}
	if l := st.lease(t); l.Epoch != 10 {
		t.Fatalf("lease epoch %d; want 10", l.Epoch)
	}
}

// TestCorruptedLeaseDoesNotRestartTheEpochs covers an unreadable lease: once it
// expired, the instance takes it over above the snapshots' epochs, and the lease is
// not counted as absent.
func TestCorruptedLeaseDoesNotRestartTheEpochs(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	st.data[LeaseKey("prod")] = []byte("{corrupted")
	m := newFloorManager(t, st, clock, "fresh", 9, nil)
	m.Step(ctx)
	clock.Advance(testTTL)
	m.Step(ctx)
	if m.Epoch() != 10 || m.AcquiredFresh() {
		t.Fatalf("epoch %d (fresh %v) after a corrupted lease; want 10, not fresh", m.Epoch(), m.AcquiredFresh())
	}
}

// TestUnknownEpochFloorDefersTheAcquisition checks that a lease is not acquired
// while the snapshots on the target cannot be listed.
func TestUnknownEpochFloorDefersTheAcquisition(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	m := newFloorManager(t, st, clock, "a", 0, errors.New("list failed"))
	m.Step(ctx)
	if m.Role() != RoleStandby {
		t.Fatal("acquired the lease without knowing the highest epoch on record")
	}
	if _, _, err := st.ReadVersioned(ctx, LeaseKey("prod")); err == nil {
		t.Fatal("a lease was written")
	}
}
