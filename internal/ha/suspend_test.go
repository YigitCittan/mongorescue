package ha

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestSuspendFailsTheFence simulates a suspended machine: the monotonic clock stands
// still while the wall clock moves on by two minutes. The fencing check fails, Run
// wakes for it, and the next step steps down instead of renewing.
func TestSuspendFailsTheFence(t *testing.T) {
	ctx := context.Background()
	mono, wall, st := newFakeClock(), newFakeClock(), newMemStore()
	m, err := New(Config{Store: st, Cluster: "prod", InstanceID: "a", KeyID: "key1", TTL: testTTL, Renew: testRenew,
		Now: mono.Now, Wall: wall.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m.Step(ctx)
	if m.Check() != nil {
		t.Fatal("not active after acquiring")
	}
	wall.Advance(2 * time.Minute) // the monotonic clock is frozen
	if m.Check() == nil {
		t.Fatal("the fencing check passes after a two-minute suspend")
	}
	if got := m.untilNextStep(); got != 0 {
		t.Fatalf("Run waits %s after the suspend; want an immediate step", got)
	}
	m.Step(ctx)
	if m.Role() != RoleStandby {
		t.Fatal("renewed instead of stepping down after the suspend")
	}

	// A wall clock set back does not shorten anything on its own.
	mono2, wall2 := newFakeClock(), newFakeClock()
	m2, err := New(Config{Store: newMemStore(), Cluster: "prod", InstanceID: "b", TTL: testTTL, Renew: testRenew,
		Now: mono2.Now, Wall: wall2.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m2.Step(ctx)
	wall2.Advance(-time.Hour)
	if m2.Check() != nil {
		t.Fatal("a wall clock set back failed the fencing check")
	}
}
