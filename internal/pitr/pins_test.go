package pitr_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestPinSetPinsUntilReleased(t *testing.T) {
	pins := pitr.NewPinSet(pitr.PinOptions{})
	p1 := pins.Pin("restore 1", pitr.PinTarget{Bases: []string{"b1"}, Chunks: []string{"c1", "c2"},
		Archives: []pitr.ArchiveRef{{TargetID: "t", Key: "k1"}}})
	p2 := pins.Pin("restore 2", pitr.PinTarget{Chunks: []string{"c2"}})
	snap := pins.Snapshot()
	if !snap.Base("b1") || !snap.Chunk("c1") || !snap.Chunk("c2") || snap.Chunk("c3") || snap.Empty() ||
		!snap.Archive("t", "k1") || snap.Archive("t", "k2") || snap.Archive("u", "k1") {
		t.Fatalf("snapshot with two pins: %+v", snap)
	}
	p1.Release()
	p1.Release() // a second release does nothing
	snap = pins.Snapshot()
	if snap.Base("b1") || snap.Chunk("c1") || !snap.Chunk("c2") || snap.Archive("t", "k1") || pins.Len() != 1 {
		t.Fatalf("snapshot after one release: %+v (%d pins)", snap, pins.Len())
	}
	p2.Release()
	if !pins.Snapshot().Empty() || pins.Len() != 0 {
		t.Fatal("pins left after every release")
	}
	var none *pitr.PinSet
	h := none.Pin("x", pitr.PinTarget{Bases: []string{"b"}})
	h.Release()
	h.KeepAlive()()
	if h.Refresh() || !none.Snapshot().Empty() || none.Len() != 0 {
		t.Fatal("a nil pin set pins something")
	}
}

// fakeNow is a clock tests move by hand.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPinSetDropsAbandonedPins(t *testing.T) {
	clock := &fakeNow{t: time.Unix(1000, 0)}
	var logs bytes.Buffer
	pins := pitr.NewPinSet(pitr.PinOptions{MaxAge: time.Hour, Now: clock.Now, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	running := pins.Pin("running restore", pitr.PinTarget{Bases: []string{"b0"}})
	abandoned := pins.Pin("forgotten restore", pitr.PinTarget{Bases: []string{"b1"}, Chunks: []string{"c1"}})
	for range 3 { // the running restore refreshes its pin, the abandoned one does not
		clock.add(50 * time.Minute)
		if !running.Refresh() {
			t.Fatal("refreshing a held pin failed")
		}
	}
	snap := pins.Snapshot()
	if !snap.Base("b0") {
		t.Fatal("a refreshed pin expired after 150 minutes")
	}
	if snap.Chunk("c1") || snap.Base("b1") || len(snap.Expired) != 1 || snap.Expired[0].Owner != "forgotten restore" {
		t.Fatalf("an abandoned pin still counts: %+v", snap)
	}
	if !strings.Contains(logs.String(), "forgotten restore") || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("no warning about the abandoned pin:\n%s", logs.String())
	}
	if pins.Len() != 1 || len(pins.Snapshot().Expired) != 0 || abandoned.Refresh() {
		t.Fatal("the abandoned pin was not dropped once")
	}
	abandoned.Release() // releasing a dropped pin is harmless
	running.Release()
}

func TestPinKeepAliveRefreshes(t *testing.T) {
	pins := pitr.NewPinSet(pitr.PinOptions{MaxAge: 60 * time.Millisecond})
	pin := pins.Pin("long restore", pitr.PinTarget{Bases: []string{"b"}})
	stop := pin.KeepAlive()
	time.Sleep(300 * time.Millisecond) // five times the maximum age
	ok := pins.Snapshot().Base("b")
	stop()
	stop() // a second stop does nothing
	if !ok {
		t.Fatal("a kept-alive pin expired")
	}
	pin.Release()
}

// liveRepo is a Repository whose ListChunks returns fixed chunks.
type liveRepo struct {
	pitr.Repository
	chunks []*pitr.Chunk
}

func (r liveRepo) ListChunks(context.Context, pitr.ChunkQuery) ([]*pitr.Chunk, error) {
	return r.chunks, nil
}

func TestCheckPlanLive(t *testing.T) {
	ts := func(s uint32) pitr.Timestamp { return pitr.Timestamp{T: s, I: 1} }
	c1 := &pitr.Chunk{ID: "c1", From: ts(100), To: ts(110), Status: pitr.ChunkCommitted}
	c2 := &pitr.Chunk{ID: "c2", From: ts(110), To: ts(120), Status: pitr.ChunkCommitted}
	plan := &pitr.RestorePlan{StreamID: "s", ChainID: "ch", Chunks: []*pitr.Chunk{c1, c2}}
	if err := pitr.CheckPlanLive(context.Background(), liveRepo{chunks: []*pitr.Chunk{c1, c2}}, plan); err != nil {
		t.Fatalf("live plan: %v", err)
	}
	deleted := *c1
	at := time.Now()
	deleted.DeletedAt = &at
	err := pitr.CheckPlanLive(context.Background(), liveRepo{chunks: []*pitr.Chunk{&deleted, c2}}, plan)
	if !errors.Is(err, pitr.ErrPlanStale) {
		t.Fatalf("deleted chunk: %v", err)
	}
	if err = pitr.CheckPlanLive(context.Background(), liveRepo{chunks: []*pitr.Chunk{c1}}, plan); !errors.Is(err, pitr.ErrPlanStale) {
		t.Fatalf("missing chunk: %v", err)
	}
}
