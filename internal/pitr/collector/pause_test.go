package collector

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// TestPauseEndsTheChainAndResumeStartsANewOne checks the stream steps of an
// in-place point-in-time restore: PauseStream disables the stream, waits for its
// collector and ends its chain as stopped; the oplog written while it is paused
// joins no chain; ResumeStream enables it again and returns once the collector
// has opened a new chain, which starts after those writes.
func TestPauseEndsTheChainAndResumeStartsANewOne(t *testing.T) {
	fx := newFixture(t)
	fx.svc.cfg.Clock = realClock{}
	fx.svc.cfg.ReconcileInterval = 10 * time.Millisecond
	ctx := context.Background()
	fx.svc.Start(ctx)
	t.Cleanup(fx.svc.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for !fx.svc.Running(fx.stream.ID) || len(fx.chunks(fx.stateChain())) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the collector did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	old := fx.stateChain()

	pauseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	was, err := fx.svc.PauseStream(pauseCtx, fx.stream.ID)
	if err != nil || !was {
		t.Fatalf("PauseStream = %v, %v; want an enabled stream paused", was, err)
	}
	if fx.svc.collecting(fx.stream.ID) {
		t.Fatal("the collector still runs after PauseStream")
	}
	st, err := fx.repo.GetStream(ctx, fx.stream.ID)
	if err != nil || st.Enabled {
		t.Fatalf("stream = %+v, %v; want it disabled", st, err)
	}
	for _, c := range fx.chains() {
		if c.Open() || c.EndReason != pitr.EndStopped {
			t.Fatalf("chain %s: open %v, reason %q; want it ended as stopped", c.ChainID, c.Open(), c.EndReason)
		}
	}
	// The swap's writes.
	fx.f.add(t, 5)
	swapEnd := fx.f.newest()

	if err = fx.svc.ResumeStream(pauseCtx, fx.stream.ID); err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	st, err = fx.repo.GetStream(ctx, fx.stream.ID)
	if err != nil || !st.Enabled {
		t.Fatalf("stream = %+v, %v; want it enabled", st, err)
	}
	var open *pitr.Chain
	for _, c := range fx.chains() {
		if c.Open() {
			open = c
		}
	}
	if open == nil || open.ChainID == old || open.Start.Compare(swapEnd) < 0 {
		t.Fatalf("open chain %+v; want a new one starting at or after %s", open, swapEnd)
	}

	// Pausing a disabled stream reports it and ends nothing more.
	if _, err := fx.svc.PauseStream(pauseCtx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if was, err := fx.svc.PauseStream(pauseCtx, fx.stream.ID); err != nil || was {
		t.Fatalf("PauseStream of a disabled stream = %v, %v", was, err)
	}
	if _, err := fx.svc.PauseStream(pauseCtx, "nope"); err == nil {
		t.Fatal("PauseStream of an unknown stream succeeded")
	}
}

// TestWorkerStartsANewChainAfterAnEndedOne checks that a collector whose stored
// position belongs to an ended chain (a stream paused by an in-place restore)
// starts a new chain at the newest majority-committed entry instead of failing
// on every commit.
func TestWorkerStartsANewChainAfterAnEndedOne(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	w := fx.worker()
	fx.step(w)
	fx.f.add(t, 3)
	fx.step(w)
	old := fx.state()
	if err := fx.repo.EndChain(ctx, fx.stream.ID, old.ChainID, old.Last.TS, pitr.EndStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	fx.f.add(t, 4)
	w = fx.worker()
	fx.step(w)
	st := fx.state()
	if st.ChainID == old.ChainID || st.Last.TS != fx.f.newest() {
		t.Fatalf("state %+v; want a new chain at %s", st, fx.f.newest())
	}
	fx.f.add(t, 2)
	fx.step(w)
	if cs := fx.chunks(st.ChainID); len(cs) == 0 {
		t.Fatal("the new chain stores no chunk")
	}
}
