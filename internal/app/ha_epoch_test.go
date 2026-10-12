package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// useManager replaces the supervisor's lease manager with one on a fake clock.
func useManager(t *testing.T, sup *haSupervisor, now func() time.Time) *ha.Manager {
	t.Helper()
	ls, err := storage.AsLeaseStore(sup.rt.storage())
	if err != nil {
		t.Fatal(err)
	}
	st := sup.rt.storage()
	m, err := ha.New(ha.Config{Store: ls, Cluster: sup.cfg.HA.Cluster, InstanceID: sup.rt.state.InstanceID,
		TTL: sup.cfg.HA.LeaseTTL, Renew: sup.cfg.HA.RenewInterval, Now: now, Wall: now,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		EpochFloor: func(ctx context.Context) (uint64, error) { return sup.epochFloor(ctx, st) }})
	if err != nil {
		t.Fatal(err)
	}
	sup.rt.mu.Lock()
	sup.rt.manager = m
	sup.rt.mu.Unlock()
	return m
}

// TestOwnReleasedLeaseWithoutSnapshotIsRefused checks that a lease that existed,
// even one this instance released itself, is never taken over without a snapshot.
func TestOwnReleasedLeaseWithoutSnapshotIsRefused(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	sup := bootstrappedSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	m := sup.rt.mgr()
	m.Step(ctx) // epoch 1, created
	sup.recordActiveEpoch(m.Epoch())
	if err := m.Release(ctx); err != nil {
		t.Fatal(err)
	}
	m2 := useManager(t, sup, time.Now) // a restart: no cooldown
	m2.Step(ctx)
	if m2.Epoch() != 2 || m2.AcquiredFresh() {
		t.Fatalf("epoch %d, fresh %v; want 2, not fresh", m2.Epoch(), m2.AcquiredFresh())
	}
	if err := sup.prepareTakeover(ctx); !errors.Is(err, ErrNoSnapshotToTakeOver) {
		t.Fatalf("a takeover of an existing lease without a snapshot = %v; want ErrNoSnapshotToTakeOver", err)
	}
}

// TestCorruptedLeaseWithoutSnapshotIsRefused checks that an unreadable lease counts
// as a lease that had a holder: without a snapshot it is not taken over.
func TestCorruptedLeaseWithoutSnapshotIsRefused(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	sup := bootstrappedSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	if _, err := sup.rt.storage().Save(ctx, ha.LeaseKey(sup.cfg.HA.Cluster), strings.NewReader("{corrupted")); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	m := useManager(t, sup, func() time.Time { return clock })
	m.Step(ctx)
	clock = clock.Add(sup.cfg.HA.LeaseTTL)
	m.Step(ctx)
	if m.Role() != ha.RoleActive || m.AcquiredFresh() {
		t.Fatalf("role %s, fresh %v; want active, not fresh", m.Role(), m.AcquiredFresh())
	}
	if err := sup.prepareTakeover(ctx); !errors.Is(err, ErrNoSnapshotToTakeOver) {
		t.Fatalf("a takeover of a corrupted lease without a snapshot = %v; want ErrNoSnapshotToTakeOver", err)
	}
}

// TestNewHATargetKeepsAPopulatedDatabase covers a change of the HA target or
// cluster name: no lease and no snapshot under the new prefix, but this instance's
// database holds the metadata of its former cluster. It is kept, never swapped for
// an empty one, and the epoch continues above the one it held.
func TestNewHATargetKeepsAPopulatedDatabase(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := newTestSupervisor(t, cfg)
	sup.rt.state.Cluster, sup.rt.state.LastActiveEpoch = "old-cluster", 5
	app, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.metaStore.(*store.SQLiteStore).SaveIntegrityState(ctx, haMarker, map[string]string{"from": "old cluster"}); err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(ctx, app); err != nil {
		t.Fatal(err)
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	m := sup.rt.mgr()
	m.Step(ctx)
	if m.Epoch() != 6 || !m.AcquiredFresh() {
		t.Fatalf("epoch %d, fresh %v; want 6 (above the epoch this instance held), fresh", m.Epoch(), m.AcquiredFresh())
	}
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the takeover of a new HA target was refused: %v", err)
	}
	if backups, _ := store.DatabaseBackups(cfg.MetadataDBPath()); len(backups) != 0 {
		t.Fatalf("the database was swapped out: %v", backups)
	}
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	var v map[string]string
	if ok, loadErr := active.metaStore.(*store.SQLiteStore).LoadIntegrityState(ctx, haMarker, &v); loadErr != nil || !ok || v["from"] != "old cluster" {
		t.Fatalf("the populated database was not kept: %v %v %v", v, ok, loadErr)
	}
}
