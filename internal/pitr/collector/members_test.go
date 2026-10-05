package collector

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// clone returns a member holding a copy of f's entries.
func (f *fakeOplog) clone() *fakeOplog {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fakeOplog{entries: append([]fakeEntry(nil), f.entries...), majority: f.majority, rs: f.rs, rsID: f.rsID,
		term: f.term, failAfter: -1}
}

// noBreaks asserts that the stream still has its one open chain and raised no
// chain event.
func noBreaks(t *testing.T, fx *fixture) {
	t.Helper()
	if n := len(fx.chains()); n != 1 {
		t.Fatalf("%d chains: a secondary's view ended the chain", n)
	}
	if fx.events.count(events.PITRChainBroken) != 0 || fx.events.count(events.PITRDiverged) != 0 {
		t.Fatal("a secondary's view raised a chain event")
	}
}

func TestLaggingSecondaryIsNotPinned(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	// The secondary has not replicated the stored position yet.
	fx.secondary = fx.f.clone()
	fx.secondary.entries = fx.secondary.entries[:len(fx.secondary.entries)-1]
	fx.f.add(t, 3)
	restarted := fx.worker() // checks its position at start
	fx.step(restarted)
	noBreaks(t, fx)
	if got := checkContinuity(fx, fx.state().ChainID); len(got) != 4 {
		t.Fatalf("%d entries collected through the primary, want 4", len(got))
	}
}

func TestFreshlySyncedSecondaryDoesNotBreakTheChain(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 5)
	// A member that was just initial-synced holds only the newest entries: its
	// window starts after the position, which looks like an overrun.
	fx.secondary = fx.f.clone()
	fx.secondary.truncate(fx.f.newest())
	restarted := fx.worker()
	fx.step(restarted)
	fx.step(restarted)
	noBreaks(t, fx)
	if got := checkContinuity(fx, fx.state().ChainID); len(got) != 6 {
		t.Fatalf("%d entries, want 6", len(got))
	}
}

func TestDivergenceOnlyOnASecondaryIsIgnored(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 2)
	fx.step(w)
	// The secondary holds another history after the start (a stale rollback),
	// the primary does not.
	fx.secondary = fx.f.clone()
	fx.secondary.rewriteFrom(t, pitr.Timestamp{T: fx.state().Last.TS.T, I: 1}, 3)
	fx.f.add(t, 3)
	restarted := fx.worker()
	fx.step(restarted)
	noBreaks(t, fx)
}

func TestDivergenceOnThePrimaryStillDiverges(t *testing.T) {
	fx := newFixture(t)
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 3)
	fx.step(w)
	good := fx.state().Last.TS
	fx.f.add(t, 3)
	fx.step(w)
	fx.f.rewriteFrom(t, pitr.Timestamp{T: good.T + 1, I: 1}, 4)
	fx.secondary = fx.f.clone()
	restarted := fx.worker()
	fx.step(restarted)
	if fx.events.count(events.PITRDiverged) != 1 {
		t.Fatal("a divergence confirmed by the primary did not diverge")
	}
}
