package collector

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestRetentionRemovesOrphanChunksAfterTheGracePeriod(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.svc.cfg.DeleteGrace = func() time.Duration { return 24 * time.Hour }
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	// A crash between storing a chunk's object and committing its row.
	orphan := ChunkKey(fx.stream, fx.state().ChainID, fx.state().Last.TS, pitr.Timestamp{T: fx.state().Last.TS.T + 60, I: 1})
	if _, err := fx.storage.Save(ctx, orphan, bytes.NewReader([]byte("partial"))); err != nil {
		t.Fatal(err)
	}
	objects := fx.objects()

	// Within an hour plus the grace period it stays (the scan reports it).
	fx.clock.mu.Lock()
	fx.clock.now = time.Now().Add(12 * time.Hour)
	fx.clock.mu.Unlock()
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if fx.objects() != objects {
		t.Fatal("an orphan was removed before the grace period ended")
	}

	fx.clock.advance(14 * time.Hour)
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.storage.Stat(ctx, orphan); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the orphan stays after the grace period: %v", err)
	}
	if fx.objects() != objects-1 {
		t.Fatalf("%d objects, want %d: a committed chunk was removed", fx.objects(), objects-1)
	}
}
