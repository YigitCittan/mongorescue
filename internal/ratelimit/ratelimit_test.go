package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestBurstRefillAndSeparateKeys(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	l := New(WithClock(c.Now))
	lim := Limit{PerMinute: 60, Burst: 3}
	for i := range 3 {
		if ok, _ := l.Allow("a", lim); !ok {
			t.Fatalf("request %d within the burst refused", i)
		}
	}
	ok, wait := l.Allow("a", lim)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over the burst: %v, wait %v; want a refusal of about 1s", ok, wait)
	}
	if ok, _ := l.Allow("b", lim); !ok {
		t.Fatal("another key shares the budget")
	}
	c.now = c.now.Add(time.Second)
	if ok, _ := l.Allow("a", lim); !ok {
		t.Fatal("no token after a second at 60/min")
	}
	if ok, _ := l.Allow("a", lim); ok {
		t.Fatal("a refused request must not have been counted as a token")
	}
}

func TestDisabledAndChangedLimits(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	l := New(WithClock(c.Now))
	for range 100 {
		if ok, _ := l.Allow("a", Limit{}); !ok {
			t.Fatal("a disabled limit refused a request")
		}
	}
	if l.Len() != 0 {
		t.Fatalf("a disabled limit kept %d buckets", l.Len())
	}
	small := Limit{PerMinute: 1, Burst: 1}
	if ok, _ := l.Allow("a", small); !ok {
		t.Fatal("first request refused")
	}
	if ok, _ := l.Allow("a", small); ok {
		t.Fatal("second request at 1/min with a burst of 1 allowed")
	}
	// A raised limit applies to the existing bucket from now on: at 100/s the next
	// token comes within 10ms, not in a minute.
	big := Limit{PerMinute: 6000, Burst: 10}
	if ok, wait := l.Allow("a", big); ok || wait > 10*time.Millisecond {
		t.Fatalf("raised limit: %v, wait %v; want a refusal of at most 10ms", ok, wait)
	}
	c.now = c.now.Add(10 * time.Millisecond)
	if ok, _ := l.Allow("a", big); !ok {
		t.Fatal("the raised limit did not apply")
	}
}

func TestIdleBucketsAreEvictedAndTheCapHolds(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	l := New(WithClock(c.Now), WithMaxKeys(3000))
	lim := Limit{PerMinute: 60, Burst: 2}
	for i := range sweepSize {
		l.Allow(fmt.Sprint("old-", i), lim)
	}
	// Two seconds later every old bucket has refilled: the next new key sweeps them.
	c.now = c.now.Add(2 * time.Second)
	l.Allow("new", lim)
	if n := l.Len(); n != 1 {
		t.Fatalf("%d buckets after the sweep; want only the new one", n)
	}
	// Busy buckets are kept, up to the cap.
	for i := range 5000 {
		l.Allow(fmt.Sprint("busy-", i), lim)
	}
	if n := l.Len(); n > 3000 {
		t.Fatalf("%d buckets; the cap is 3000", n)
	}
}

func TestGatePassesOncePerInterval(t *testing.T) {
	c := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	g := NewGate(time.Minute, c.Now)
	if !g.Pass("k") || g.Pass("k") {
		t.Fatal("want the first pass only")
	}
	if !g.Pass("other") {
		t.Fatal("keys are gated separately")
	}
	c.now = c.now.Add(time.Minute)
	if !g.Pass("k") {
		t.Fatal("want a pass after the interval")
	}
	// Ten minutes of 1000 new keys each: the keys of earlier minutes are forgotten.
	for round := range 10 {
		for i := range 1000 {
			g.Pass(fmt.Sprint(round, "-", i))
		}
		c.now = c.now.Add(time.Minute)
	}
	g.mu.Lock()
	n := len(g.last)
	g.mu.Unlock()
	if n > 4*sweepSize {
		t.Fatalf("%d gated keys kept; the keys of past intervals must be swept", n)
	}
}
