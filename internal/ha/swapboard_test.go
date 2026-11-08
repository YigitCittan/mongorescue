package ha

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func newTestBoard(t *testing.T, st storage.Storage, key string, fence models.Fence) *SwapBoard {
	t.Helper()
	b, err := NewSwapBoard("prod", st, []byte(strings.Repeat(key, 32)), fence, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testMarker(id string, epoch uint64) models.SwapMarker {
	return models.SwapMarker{RestoreID: id, StreamID: "str_1", ConnectionID: "conn_1", Databases: []string{"shop"},
		Clones: []string{"shop_rescue_x"}, StreamWasEnabled: true, Epoch: epoch}
}

func TestSwapBoardLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBoard(t, st, "a", &staticFence{epoch: 3})
	if err = b.Begin(ctx, testMarker("rst_1", 3)); err != nil {
		t.Fatal(err)
	}
	if err = b.Begin(ctx, testMarker("rst_1", 3)); !errors.Is(err, models.ErrSwapMarkerExists) {
		t.Fatalf("second Begin = %v; want ErrSwapMarkerExists", err)
	}
	open, err := b.Open(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("Open = %v, %v; want one marker", open, err)
	}
	if m := open[0]; m.Finished() || m.Phase != models.SwapPhaseSwapping || m.Epoch != 3 || !m.StreamWasEnabled || m.StartedAt.IsZero() {
		t.Fatalf("marker = %+v; want swapping under epoch 3", m)
	}
	if err = b.Finish(ctx, "rst_1", models.SwapCompleted, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	// Finishing twice changes nothing.
	if err = b.Finish(ctx, "rst_1", models.SwapFailed, nil); err != nil {
		t.Fatal(err)
	}
	open, _ = b.Open(ctx)
	if len(open) != 1 || !open[0].Finished() || open[0].Outcome != models.SwapCompleted || len(open[0].Swapped) != 1 {
		t.Fatalf("finished marker = %+v", open)
	}
	if err = b.Resolve(ctx, "rst_1"); err != nil {
		t.Fatal(err)
	}
	if err = b.Resolve(ctx, "rst_1"); err != nil {
		t.Fatalf("Resolve of a missing marker = %v", err)
	}
	if open, _ = b.Open(ctx); len(open) != 0 {
		t.Fatalf("markers after Resolve = %+v", open)
	}
}

// A swap whose lease is lost right after its marker was written never starts:
// Begin fails with the fence's error and leaves the marker aborted.
func TestSwapBoardBeginFencedAbortsTheMarker(t *testing.T) {
	ctx := context.Background()
	st, _ := storage.NewLocalStorage(t.TempDir())
	b := newTestBoard(t, st, "a", &staticFence{epoch: 3, err: ErrNotActive})
	if err := b.Begin(ctx, testMarker("rst_1", 3)); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Begin = %v; want ErrNotActive", err)
	}
	open, err := b.Open(ctx)
	if err != nil || len(open) != 1 || !open[0].Finished() || open[0].Outcome != models.SwapAborted {
		t.Fatalf("markers = %+v, %v; want one aborted marker", open, err)
	}
}

// Only a holder of secret.key writes a marker that counts: an object with another
// key, an altered one and one moved to another restore's key are all ignored.
func TestSwapBoardIgnoresMarkersThatAreNotAuthentic(t *testing.T) {
	ctx := context.Background()
	st, _ := storage.NewLocalStorage(t.TempDir())
	good := newTestBoard(t, st, "a", &staticFence{epoch: 1})
	other := newTestBoard(t, st, "b", &staticFence{epoch: 1})
	if err := other.Begin(ctx, testMarker("rst_foreign", 1)); err != nil {
		t.Fatal(err)
	}
	if err := good.Begin(ctx, testMarker("rst_real", 1)); err != nil {
		t.Fatal(err)
	}
	// Altered content: the phase flipped without a new MAC.
	data, _, err := good.leases.ReadVersioned(ctx, SwapKey("prod", "rst_real"))
	if err != nil {
		t.Fatal(err)
	}
	altered := bytes.ReplaceAll(data, []byte(`"phase":"swapping"`), []byte(`"phase":"finished"`))
	if bytes.Equal(altered, data) {
		t.Fatalf("test setup: %s", data)
	}
	if _, err = good.leases.CreateIfAbsent(ctx, SwapKey("prod", "rst_altered"), altered); err != nil {
		t.Fatal(err)
	}
	// A genuine marker copied to another restore's key.
	if _, err = good.leases.CreateIfAbsent(ctx, SwapKey("prod", "rst_moved"), data); err != nil {
		t.Fatal(err)
	}
	open, err := good.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].RestoreID != "rst_real" || open[0].Finished() {
		t.Fatalf("open markers = %+v; want only the genuine rst_real", open)
	}
	// Finish refuses to act on an object it cannot authenticate.
	if err = good.Finish(ctx, "rst_foreign", models.SwapCompleted, nil); !errors.Is(err, models.ErrSwapLedger) {
		t.Fatalf("Finish of a foreign marker = %v; want ErrSwapLedger", err)
	}
}

func TestSwapBoardRejectsUnsafeIDsAndStorageErrors(t *testing.T) {
	ctx := context.Background()
	st, _ := storage.NewLocalStorage(t.TempDir())
	b := newTestBoard(t, st, "a", &staticFence{epoch: 1})
	for _, id := range []string{"", "../lease", "a/b", ".hidden", strings.Repeat("x", 200)} {
		if err := b.Begin(ctx, testMarker(id, 1)); !errors.Is(err, models.ErrSwapLedger) {
			t.Errorf("Begin(%q) = %v; want ErrSwapLedger", id, err)
		}
	}
	if _, err := NewSwapBoard("Bad Name", st, make([]byte, 32), &staticFence{}, nil); err == nil {
		t.Error("an invalid cluster name was accepted")
	}
	if _, err := NewSwapBoard("prod", storage.NewMockStorage(), make([]byte, 32), &staticFence{}, nil); !errors.Is(err, storage.ErrConditionalWritesUnsupported) {
		t.Errorf("a storage without conditional writes: %v", err)
	}
}
