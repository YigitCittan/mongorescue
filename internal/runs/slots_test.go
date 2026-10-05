package runs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAcquireSlotQueuesInOrder(t *testing.T) {
	m := NewManager(nil)
	defer func() { _ = m.Shutdown(context.Background()) }()
	key := ConnectionKey("conn_a")
	ctx := context.Background()

	first, err := m.AcquireSlot(ctx, key, 1, func() { t.Error("the first backup must not wait") })
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		order  []int
		waited atomic.Int32
		wg     sync.WaitGroup
	)
	releases := make(chan func(), 3)
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := m.AcquireSlot(ctx, key, 1, func() { waited.Add(1) })
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			releases <- release
		}()
		// Queue them one after the other.
		waitFor(t, "the waiter to queue", func() bool { return m.SlotWaiters(key) == i })
	}
	if waited.Load() != 3 {
		t.Fatalf("%d callers reported waiting, want 3", waited.Load())
	}
	first()
	first() // idempotent: frees one slot only
	for i := 0; i < 3; i++ {
		release := <-releases
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n != i+1 {
			t.Fatalf("%d backups hold the single slot", n)
		}
		release()
	}
	wg.Wait()
	if order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("served in order %v, want 1 2 3", order)
	}
	if m.SlotWaiters(key) != 0 || len(m.slots) != 0 {
		t.Fatalf("slot state left behind: %+v", m.slots)
	}
}

func TestAcquireSlotLimitAndKeys(t *testing.T) {
	m := NewManager(nil)
	defer func() { _ = m.Shutdown(context.Background()) }()
	ctx := context.Background()
	a, b := ConnectionKey("conn_a"), ConnectionKey("conn_b")
	r1, _ := m.AcquireSlot(ctx, a, 2, nil)
	r2, _ := m.AcquireSlot(ctx, a, 2, nil)
	// Another connection has its own slots, and no limit never waits.
	rb, err := m.AcquireSlot(ctx, b, 1, func() { t.Error("conn_b must not wait") })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcquireSlot(ctx, a, 0, func() { t.Error("no limit must not wait") }); err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		r, _ := m.AcquireSlot(ctx, a, 2, nil)
		got <- r
	}()
	waitFor(t, "the third backup of conn_a to wait", func() bool { return m.SlotWaiters(a) == 1 })
	// Raising the limit lets the waiter in at once.
	r4, err := m.AcquireSlot(ctx, a, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	(<-got)()
	for _, r := range []func(){r1, r2, rb, r4} {
		r()
	}
}

func TestAcquireSlotCancelledWhileWaiting(t *testing.T) {
	m := NewManager(nil)
	key := ConnectionKey("conn_a")
	held, _ := m.AcquireSlot(context.Background(), key, 1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.AcquireSlot(ctx, key, 1, nil)
		done <- err
	}()
	waitFor(t, "the waiter", func() bool { return m.SlotWaiters(key) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	if m.SlotWaiters(key) != 0 {
		t.Fatal("a cancelled waiter stays queued")
	}
	held()
	// The slot is free again.
	r, err := m.AcquireSlot(context.Background(), key, 1, func() { t.Error("must not wait") })
	if err != nil {
		t.Fatal(err)
	}
	r()

	// Shutdown ends every wait.
	held, _ = m.AcquireSlot(context.Background(), key, 1, nil)
	go func() {
		_, err := m.AcquireSlot(context.Background(), key, 1, nil)
		done <- err
	}()
	waitFor(t, "the waiter", func() bool { return m.SlotWaiters(key) == 1 })
	_ = m.Shutdown(context.Background())
	if err := <-done; !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("wait during shutdown = %v", err)
	}
	held()
	if _, err := m.AcquireSlot(context.Background(), key, 1, nil); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("acquire after shutdown = %v", err)
	}
}
