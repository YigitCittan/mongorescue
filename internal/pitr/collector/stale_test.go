package collector

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// staleSession is a fakeSession whose credentials can end (see StaleSession).
type staleSession struct {
	fakeSession
	stale  *atomic.Bool
	closed *atomic.Int32
}

func (s staleSession) Stale(context.Context) bool { return s.stale.Load() }
func (s staleSession) Close()                     { s.closed.Add(1) }

// TestStaleSessionReconnects checks that a session whose credentials end (a
// Vault lease, rotated static credentials) is replaced before the next step,
// without breaking the chain.
func TestStaleSessionReconnects(t *testing.T) {
	fx := newFixture(t)
	var opens, closed atomic.Int32
	var stale atomic.Bool
	fx.svc.cfg.Open = func(context.Context, *pitr.Stream) (Session, error) {
		opens.Add(1)
		stale.Store(false)
		return staleSession{fakeSession: fakeSession{members: []*fakeOplog{fx.f}}, stale: &stale, closed: &closed}, nil
	}
	w := fx.worker()
	fx.step(w)
	chain := fx.state().ChainID
	fx.f.add(t, 3)
	fx.step(w)
	if opens.Load() != 1 || closed.Load() != 0 {
		t.Fatalf("opens %d, closed %d; want one session", opens.Load(), closed.Load())
	}
	stale.Store(true)
	fx.f.add(t, 3)
	fx.step(w)
	if opens.Load() != 2 || closed.Load() != 1 {
		t.Fatalf("opens %d, closed %d after the credentials ended; want a new session", opens.Load(), closed.Load())
	}
	if st := fx.state(); st.ChainID != chain || st.Last.TS != fx.f.newest() {
		t.Fatalf("the reconnect broke the chain: %+v", st)
	}
	if got := checkContinuity(fx, chain); len(got) != 7 {
		t.Fatalf("entries %v", got)
	}
}
