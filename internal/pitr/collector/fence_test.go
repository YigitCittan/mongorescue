package collector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// switchFence is a fence whose outcome a test flips.
type switchFence struct{ lost atomic.Bool }

func (f *switchFence) Check() error {
	if f.lost.Load() {
		return errors.New("this instance does not hold the HA lease")
	}
	return nil
}
func (f *switchFence) Epoch() uint64 { return 1 }

// TestNoChunkWithoutTheFence checks that a collector that lost the HA lease
// uploads no chunk, commits none and deletes nothing.
func TestNoChunkWithoutTheFence(t *testing.T) {
	fx := newFixture(t)
	fence := &switchFence{}
	fx.svc.cfg.Fence = fence
	w := fx.worker()
	fx.step(w) // starts the chain
	chain := fx.state().ChainID
	before := len(fx.chunks(chain))
	objects, err := fx.storage.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	fence.lost.Store(true)
	fx.f.add(t, 5)
	if _, err = w.step(context.Background()); err == nil {
		t.Fatal("a step without the HA lease succeeded")
	}
	if got := len(fx.chunks(chain)); got != before {
		t.Fatalf("%d chunks committed without the HA lease", got-before)
	}
	after, err := fx.storage.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(objects) {
		t.Fatalf("objects changed without the HA lease: %d before, %d after", len(objects), len(after))
	}
}
