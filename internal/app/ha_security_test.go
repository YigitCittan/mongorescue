package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// kickRecorder counts Kick calls (a fake snapshotter).
type kickRecorder struct{ n atomic.Int32 }

// Kick records one call.
func (k *kickRecorder) Kick() { k.n.Add(1) }

// TestSecurityEventsAndSettingsKickTheSnapshotter checks that key rotation and
// wrapping events, auth changes and settings changes ask for a snapshot, and that
// unrelated events do not.
func TestSecurityEventsAndSettingsKickTheSnapshotter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var k kickRecorder
	onEvent := kickOnFinishedRun(&k)
	for _, typ := range []events.EventType{events.BackupSucceeded, events.RestoreFailed, events.SecurityKeyRotated, events.SecurityKeyWrapping} {
		before := k.n.Load()
		onEvent(ctx, events.Event{Type: typ})
		if k.n.Load() != before+1 {
			t.Errorf("%s did not kick the snapshotter", typ)
		}
	}
	before := k.n.Load()
	onEvent(ctx, events.Event{Type: events.SecurityDestructiveAction})
	if k.n.Load() != before {
		t.Error("an unrelated event kicked the snapshotter")
	}

	meta := storetest.New(t)
	authSvc, err := auth.NewService(meta, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	settingsSvc, err := settings.NewService(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	wireSecurityKicks(&k, authSvc, settingsSvc)

	before = k.n.Load()
	grace := 9
	if _, err = settingsSvc.Update(ctx, settings.Patch{Security: &settings.SecurityPatch{DeleteGraceDays: &grace}}); err != nil {
		t.Fatal(err)
	}
	if k.n.Load() == before {
		t.Error("a settings change did not kick the snapshotter")
	}

	if _, err = authSvc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	before = k.n.Load()
	if _, err = authSvc.Setup(ctx, "192.0.2.10", authSvc.SetupCode(), "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if k.n.Load() == before {
		t.Error("an auth change did not kick the snapshotter")
	}
}

// TestTakeoverAfterARevocationRejectsTheRevokedKey revokes an API key on the active
// instance, which asks the snapshotter for a snapshot (wireSecurityKicks) long
// before the snapshot interval, then releases the lease: the instance that takes
// over must not revive the key.
func TestTakeoverAfterARevocationRejectsTheRevokedKey(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	shared, key := t.TempDir(), haTestKey(t)
	w := newHAWriter(t, shared, key)
	ctx := context.Background()

	authSvc, err := auth.NewService(w.meta, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	settingsSvc, err := settings.NewService(ctx, w.meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authSvc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := authSvc.Setup(ctx, "192.0.2.10", authSvc.SetupCode(), "admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := authSvc.AuthenticateSession(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	apiKey, plain, err := authSvc.CreateAPIKey(ctx, admin, "ci", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}

	// The snapshot loop (interval 5 minutes) writes a first snapshot holding the key.
	loopCtx, stop := context.WithCancel(ctx)
	var loop sync.WaitGroup
	loop.Go(func() { w.snap.Run(loopCtx) })
	t.Cleanup(func() { stop(); loop.Wait() })
	waitWritten := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for w.written.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d snapshots written; want %d", w.written.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitWritten(1)

	// Wired as the application does: the revocation, not the interval, writes the
	// second snapshot.
	wireSecurityKicks(w.snap, authSvc, settingsSvc)
	if err = authSvc.DeleteAPIKey(ctx, admin, apiKey.ID); err != nil {
		t.Fatal(err)
	}
	waitWritten(2)
	stop()
	loop.Wait()
	w.release(t)

	sup := standbyThatAcquired(t, shared, key, 2)
	if err = sup.prepareTakeover(ctx); err != nil {
		t.Fatalf("the takeover was refused: %v", err)
	}
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	taken, err := auth.NewService(active.metaStore.(*store.SQLiteStore), auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taken.AuthenticateAPIKey(ctx, plain); err == nil {
		t.Fatal("the revoked API key works after the takeover")
	}
	if _, err = taken.AuthenticateSession(ctx, res.Token); err != nil {
		t.Fatalf("the admin session did not survive the takeover: %v", err)
	}
}
