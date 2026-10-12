package sharedcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// fakeClock is a settable clock for the TTL.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// counter computes its call count as the value; while gate is set, computations
// wait for it.
type counter struct {
	calls atomic.Int32
	gate  chan struct{}
	fail  atomic.Pointer[error]
}

func (c *counter) compute(context.Context) (int, error) {
	n := int(c.calls.Add(1))
	if c.gate != nil {
		<-c.gate
	}
	if err := c.fail.Swap(nil); err != nil {
		return n, *err
	}
	return n, nil
}

func newCache(opts Options) (*Cache[string, int], *fakeClock) {
	c := New[string, int](opts)
	clk := &fakeClock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	c.now = clk.now
	return c, clk
}

func get(t *testing.T, c *Cache[string, int], key string, stamp int64, cnt *counter) int {
	t.Helper()
	v, err := c.Get(context.Background(), key, stamp, cnt.compute)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCacheKeysStampsTTLAndInvalidation(t *testing.T) {
	c, clk := newCache(Options{TTL: time.Minute})
	cnt := &counter{}
	if v := get(t, c, "*", 1, cnt); v != 1 {
		t.Fatalf("first value %d", v)
	}
	if v := get(t, c, "*", 1, cnt); v != 1 {
		t.Fatalf("cached value %d; want 1", v)
	}
	// Another key (another caller's access) is computed apart.
	if v := get(t, c, "c1", 1, cnt); v != 2 {
		t.Fatalf("another key: %d; want its own computation", v)
	}
	// A new stamp (a write) recomputes.
	if v := get(t, c, "*", 2, cnt); v != 3 {
		t.Fatalf("new stamp: %d; want a computation", v)
	}
	// Invalidation recomputes every key.
	c.Invalidate()
	if v := get(t, c, "*", 2, cnt); v != 4 {
		t.Fatalf("after Invalidate: %d", v)
	}
	if v := get(t, c, "c1", 1, cnt); v != 5 {
		t.Fatalf("other key after Invalidate: %d", v)
	}
	// The TTL expires a value.
	clk.add(time.Minute)
	if v := get(t, c, "*", 2, cnt); v != 6 {
		t.Fatalf("after the TTL: %d", v)
	}
}

func TestCacheComputesOnceForConcurrentCallers(t *testing.T) {
	c, _ := newCache(Options{TTL: time.Hour})
	cnt := &counter{gate: make(chan struct{})}
	var wg sync.WaitGroup
	got := make([]int, 16)
	for i := range got {
		wg.Go(func() { got[i] = get(t, c, "*", 7, cnt) })
	}
	for cnt.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(cnt.gate)
	wg.Wait()
	if n := cnt.calls.Load(); n != 1 {
		t.Fatalf("%d computations for 16 callers; want 1", n)
	}
	for _, v := range got {
		if v != 1 {
			t.Fatalf("values %v", got)
		}
	}
}

func TestCacheDoesNotKeepFailures(t *testing.T) {
	c, _ := newCache(Options{TTL: time.Hour, ServeStale: true})
	cnt := &counter{}
	boom := errors.New("boom")
	cnt.fail.Store(&boom)
	if _, err := c.Get(context.Background(), "*", 0, cnt.compute); !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
	if v := get(t, c, "*", 0, cnt); v != 2 {
		t.Fatalf("after a failure: %d; want a new computation", v)
	}
}

// TestCacheServesStaleOnlyAfterTheTTL checks that with ServeStale a caller asking
// while an expired value is recomputed gets the previous value, but never after an
// invalidation or a new stamp.
func TestCacheServesStaleOnlyAfterTheTTL(t *testing.T) {
	c, clk := newCache(Options{TTL: time.Minute, ServeStale: true})
	cnt := &counter{}
	get(t, c, "*", 1, cnt)
	clk.add(time.Minute)

	refresh := func(stamp int64) (other int) {
		cnt.gate = make(chan struct{})
		done := make(chan int)
		go func() { done <- get(t, c, "*", stamp, cnt) }()
		for int(cnt.calls.Load()) < 2 {
			time.Sleep(time.Millisecond)
		}
		other = get(t, c, "*", stamp, &counter{})
		close(cnt.gate)
		<-done
		cnt.gate = nil
		return other
	}
	// Expired, same stamp: the other caller gets the previous value at once.
	if v := refresh(1); v != 1 {
		t.Fatalf("while refreshing an expired value: %d; want the previous 1", v)
	}
	if v := get(t, c, "*", 1, cnt); v != 2 {
		t.Fatalf("after the refresh: %d; want 2", v)
	}

	// A new stamp: the other caller waits for the new value instead.
	cnt.calls.Store(1)
	cnt.gate = make(chan struct{})
	done := make(chan int)
	go func() { done <- get(t, c, "*", 9, cnt) }()
	for cnt.calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	waiter := make(chan int)
	go func() { waiter <- get(t, c, "*", 9, &counter{}) }()
	select {
	case v := <-waiter:
		t.Fatalf("a caller got %d while the value of a new stamp was computed", v)
	case <-time.After(20 * time.Millisecond):
	}
	close(cnt.gate)
	if v, w := <-done, <-waiter; v != 2 || w != 2 {
		t.Fatalf("new stamp: %d and %d; want the new value 2 for both", v, w)
	}
}

func TestCacheWaiterGivesUpWithItsContext(t *testing.T) {
	c, _ := newCache(Options{TTL: time.Hour})
	cnt := &counter{gate: make(chan struct{})}
	defer close(cnt.gate)
	go func() { _, _ = c.Get(context.Background(), "*", 0, cnt.compute) }()
	for cnt.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "*", 0, cnt.compute); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter returned %v", err)
	}
}

func TestCacheBoundsItsEntries(t *testing.T) {
	c, _ := newCache(Options{TTL: time.Hour, MaxEntries: 2})
	cnt := &counter{}
	for _, k := range []string{"a", "b", "c", "d"} {
		get(t, c, k, 0, cnt)
	}
	if n := len(c.entries); n > 2 {
		t.Fatalf("%d entries; want at most 2", n)
	}
}

func TestAccessKey(t *testing.T) {
	for _, tc := range []struct {
		set  auth.ConnectionSet
		want string
	}{
		{nil, "*"},
		{auth.OnlyConnections(), ""},
		{auth.OnlyConnections("c2", "c1"), "c1,c2"},
	} {
		if got := AccessKey(tc.set); got != tc.want {
			t.Errorf("AccessKey(%v) = %q; want %q", tc.set.IDs(), got, tc.want)
		}
	}
}

// TestCachePanickingComputeFailsTheEntry checks that a computation that panics
// wakes its waiters (they compute their own), passes the panic on and is not
// cached.
func TestCachePanickingComputeFailsTheEntry(t *testing.T) {
	c, _ := newCache(Options{TTL: time.Hour, ServeStale: true})
	gate := make(chan struct{})
	started := make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = c.Get(context.Background(), "*", 0, func(context.Context) (int, error) {
			close(started)
			<-gate
			panic("boom")
		})
	}()
	<-started
	waiter := make(chan int, 1)
	go func() {
		v, err := c.Get(context.Background(), "*", 0, func(context.Context) (int, error) { return 7, nil })
		if err != nil {
			t.Error(err)
		}
		waiter <- v
	}()
	time.Sleep(20 * time.Millisecond)
	close(gate)
	if r := <-panicked; r != "boom" {
		t.Fatalf("recovered %v; want the panic passed on", r)
	}
	select {
	case v := <-waiter:
		if v != 7 {
			t.Fatalf("the waiter got %d; want its own computation", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never woke up")
	}
	cnt := &counter{}
	if v := get(t, c, "*", 0, cnt); v != 1 || cnt.calls.Load() != 1 {
		t.Fatalf("after the panic: %d (%d computations); want a new computation", v, cnt.calls.Load())
	}
}
