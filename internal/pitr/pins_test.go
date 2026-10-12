package pitr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestPinSetPinsUntilReleased(t *testing.T) {
	pins := pitr.NewPinSet(0, nil)
	r1 := pins.Pin("restore 1", []string{"b1"}, []string{"c1", "c2"})
	r2 := pins.Pin("restore 2", nil, []string{"c2"})
	snap := pins.Snapshot()
	if !snap.Base("b1") || !snap.Chunk("c1") || !snap.Chunk("c2") || snap.Chunk("c3") || snap.Empty() {
		t.Fatalf("snapshot with two pins: %+v", snap)
	}
	r1()
	r1() // a second release does nothing
	snap = pins.Snapshot()
	if snap.Base("b1") || snap.Chunk("c1") || !snap.Chunk("c2") || pins.Len() != 1 {
		t.Fatalf("snapshot after one release: %+v (%d pins)", snap, pins.Len())
	}
	r2()
	if !pins.Snapshot().Empty() || pins.Len() != 0 {
		t.Fatal("pins left after every release")
	}
	var none *pitr.PinSet
	none.Pin("x", []string{"b"}, nil)()
	if !none.Snapshot().Empty() || none.Len() != 0 {
		t.Fatal("a nil pin set pins something")
	}
}

func TestPinSetDropsStalePins(t *testing.T) {
	now := time.Unix(1000, 0)
	pins := pitr.NewPinSet(time.Hour, func() time.Time { return now })
	release := pins.Pin("forgotten restore", []string{"b1"}, []string{"c1"})
	now = now.Add(59 * time.Minute)
	if !pins.Snapshot().Chunk("c1") {
		t.Fatal("a young pin does not count")
	}
	now = now.Add(2 * time.Minute)
	snap := pins.Snapshot()
	if snap.Chunk("c1") || snap.Base("b1") || len(snap.Expired) != 1 || snap.Expired[0].Owner != "forgotten restore" {
		t.Fatalf("a pin older than its maximum age still counts: %+v", snap)
	}
	if pins.Len() != 0 || len(pins.Snapshot().Expired) != 0 {
		t.Fatal("the stale pin was not dropped once")
	}
	release() // releasing a dropped pin is harmless
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
