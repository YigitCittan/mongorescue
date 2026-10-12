package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// bootstrappedSupervisor returns a supervisor of cfg after its bootstrap (its
// standby App closed again).
func bootstrappedSupervisor(t *testing.T, cfg *config.Config) *haSupervisor {
	t.Helper()
	sup := newTestSupervisor(t, cfg)
	app, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	return sup
}

// TestTakeoverWithoutASnapshot checks that a standby takes over without a metadata
// snapshot only on the cluster's first activation: once the lease had a holder, a
// takeover without the holder's snapshot would lose its metadata, so it is refused
// and counted.
func TestTakeoverWithoutASnapshot(t *testing.T) {
	ctx := context.Background()
	shared, key := t.TempDir(), haTestKey(t)

	// First activation: the lease never had a holder (epoch 1).
	first := bootstrappedSupervisor(t, haTestConfig(t, shared, key))
	first.rt.mgr().Step(ctx)
	if first.rt.mgr().Epoch() != 1 {
		t.Fatalf("epoch %d; want 1", first.rt.mgr().Epoch())
	}
	if err := first.prepareTakeover(ctx); err != nil {
		t.Fatalf("the first activation without a snapshot was refused: %v", err)
	}
	// It goes away without writing a snapshot and releases the lease.
	if err := first.rt.mgr().Release(ctx); err != nil {
		t.Fatal(err)
	}

	// Epoch 2 without any snapshot: refused.
	second := bootstrappedSupervisor(t, haTestConfig(t, shared, key))
	second.rt.mgr().Step(ctx)
	if second.rt.mgr().Epoch() != 2 {
		t.Fatalf("epoch %d; want 2", second.rt.mgr().Epoch())
	}
	if err := second.prepareTakeover(ctx); !errors.Is(err, ErrNoSnapshotToTakeOver) {
		t.Fatalf("a takeover without the previous holder's snapshot = %v; want ErrNoSnapshotToTakeOver", err)
	}
	app, err := second.takeOver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.ha.active || second.rt.mgr().Role() != ha.RoleStandby {
		t.Fatal("the instance became active without a snapshot to take over from")
	}
	if st := second.rt.metricsState(); st.TakeoverFailures != 1 {
		t.Fatalf("takeover failures = %d; want 1", st.TakeoverFailures)
	}
}

// TestTakeoverWithoutASnapshotAfterOwnEpoch checks the other allowed case: the
// instance held the epoch right before, so nobody worked in between and its own
// database is the newest.
func TestTakeoverWithoutASnapshotAfterOwnEpoch(t *testing.T) {
	ctx := context.Background()
	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := bootstrappedSupervisor(t, cfg)
	m := sup.rt.mgr()
	m.Step(ctx) // epoch 1
	sup.recordActiveEpoch(m.Epoch())
	if err := m.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// Reacquire after the cooldown with a manager without one (a restart).
	ls, err := storage.AsLeaseStore(sup.rt.storage())
	if err != nil {
		t.Fatal(err)
	}
	m2, err := ha.New(ha.Config{Store: ls, Cluster: cfg.HA.Cluster, InstanceID: sup.rt.state.InstanceID, TTL: cfg.HA.LeaseTTL,
		Renew: cfg.HA.RenewInterval, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m2.Step(ctx)
	if m2.Epoch() != 2 {
		t.Fatalf("epoch %d; want 2", m2.Epoch())
	}
	sup.rt.mu.Lock()
	sup.rt.manager = m2
	sup.rt.mu.Unlock()
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("a takeover right after this instance's own epoch was refused: %v", err)
	}
}
