package ha

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestFirstSnapshotRightAfterActivation checks that a new active instance writes a
// snapshot as soon as its snapshot loop starts, not after the minimum gap or the
// interval: its epoch must have a snapshot before it can die.
func TestFirstSnapshotRightAfterActivation(t *testing.T) {
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newTestSnapshotter(t, st, &staticFence{epoch: 3}, &fileVacuum{content: "x"}, testPassphrase(t), newFakeClock())
	s.cfg.MinGap, s.cfg.Interval = time.Hour, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := ListSnapshots(context.Background(), st, "prod"); len(list) == 1 && list[0].Epoch == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no snapshot right after the snapshot loop started")
}
