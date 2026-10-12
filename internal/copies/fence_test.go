package copies_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/copies"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// lostFence is a fence of an instance that lost the HA lease.
type lostFence struct{}

var errLost = errors.New("this instance does not hold the HA lease")

func (lostFence) Check() error  { return errLost }
func (lostFence) Epoch() uint64 { return 0 }

func TestNoCopyWithoutTheFence(t *testing.T) {
	ctx := context.Background()
	dst := storage.NewMockStorage()
	drivers := map[string]storage.Storage{"primary": primary(t, archive), "copy": dst}
	svc := copies.New(copies.Config{Storages: func(_ context.Context, id string) (storage.Storage, error) { return drivers[id], nil },
		Fence: lostFence{}, SyncBackoff: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err := svc.CopyAll(ctx, record("copy"), 0); !errors.Is(err, errLost) {
		t.Fatalf("CopyAll without the HA lease = %v; want the fence's error", err)
	}
	if _, err := dst.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a copy was written without the HA lease: %v", err)
	}
}
