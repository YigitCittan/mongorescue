package collector

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestGapsBackOffAndAlertAtMostEveryTenMinutes(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	overrun := func() time.Duration {
		t.Helper()
		// The oplog is overrun again before the collector reads its new chain.
		fx.f.add(t, 30)
		fx.f.truncate(pitr.Timestamp{T: fx.f.newest().T - 2, I: 1})
		return fx.step(w)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, d := range want {
		if got := overrun(); got != d {
			t.Fatalf("wait after gap %d = %s, want %s", i+1, got, d)
		}
		fx.clock.advance(time.Minute)
	}
	if n := fx.events.count(events.PITRChainBroken); n != 1 {
		t.Fatalf("%d chain_broken events within ten minutes, want 1", n)
	}
	fx.clock.advance(5 * time.Minute)
	overrun()
	if n := fx.events.count(events.PITRChainBroken); n != 2 {
		t.Fatalf("%d chain_broken events after ten minutes, want 2", n)
	}
	// A tick that catches up resets the backoff.
	for range 10 {
		if d := fx.step(w); d == time.Minute {
			break
		}
	}
	if got := overrun(); got != time.Minute {
		t.Fatalf("wait after a gap following a caught-up tick = %s", got)
	}
}
