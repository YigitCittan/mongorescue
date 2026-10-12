package ha

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestStepDownExactlyAtTheDeadline covers a renewal that fails fast just before the
// deadline (ttl 21s, renew 10s: the deadline is 11s after the last renewal). Run
// wakes at the deadline instead of a renew interval later, so the step-down happens
// at S+ttl-renew, before the standby may take over at S+ttl.
func TestStepDownExactlyAtTheDeadline(t *testing.T) {
	const ttl, renew = 21 * time.Second, 10 * time.Second
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	mk := func(id string) *Manager {
		m, err := New(Config{Store: st, Cluster: "prod", InstanceID: id, KeyID: "key1", TTL: ttl, Renew: renew,
			Now: clock.Now, Wall: clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := mk("a"), mk("b")
	a.Step(ctx)
	renewedAt := clock.Now()
	b.Step(ctx)
	if got := a.untilNextStep(); got != renew {
		t.Fatalf("after a renewal Run waits %s; want the renew interval", got)
	}

	clock.Advance(renew)
	st.setFail(true)
	a.Step(ctx) // fails fast
	if a.Role() != RoleActive {
		t.Fatal("stepped down before the deadline")
	}
	wait := a.untilNextStep()
	if want := ttl - renew - renew; wait != want {
		t.Fatalf("after a failed renewal Run waits %s; want %s, the time to the deadline", wait, want)
	}
	clock.Advance(wait)
	if a.Check() == nil {
		t.Fatal("the fencing check passes at the deadline")
	}
	a.Step(ctx)
	if a.Role() != RoleStandby {
		t.Fatal("no step-down at the deadline")
	}
	stepDownAt := clock.Now()
	if stepDownAt.Sub(renewedAt) != ttl-renew {
		t.Fatalf("stepped down %s after the last renewal; want %s", stepDownAt.Sub(renewedAt), ttl-renew)
	}

	st.setFail(false)
	for b.Role() != RoleActive {
		clock.Advance(time.Second)
		b.Step(ctx)
	}
	if !stepDownAt.Before(clock.Now()) || clock.Now().Sub(renewedAt) < ttl {
		t.Fatalf("the standby took over %s after the renewal, the active stepped down after %s",
			clock.Now().Sub(renewedAt), stepDownAt.Sub(renewedAt))
	}
}
