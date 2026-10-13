package ha

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestStepDownExactlyAtTheDeadline covers renewals that fail fast until the
// deadline (ttl 25s, renew 10s: the deadline is 15s after the last renewal). After
// the first failure Run retries every retry pause, never past the deadline, so the
// step-down happens exactly at S+ttl-renew, before the standby may take over at
// S+ttl.
func TestStepDownExactlyAtTheDeadline(t *testing.T) {
	const ttl, renew = 25 * time.Second, 10 * time.Second
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
	attempts := 0
	for a.Role() == RoleActive {
		a.Step(ctx) // fails fast, or steps down at the deadline
		attempts++
		if a.Role() != RoleActive {
			break
		}
		wait := a.untilNextStep()
		if left := renewedAt.Add(ttl - renew).Sub(clock.Now()); wait != min(renew/10, left) {
			t.Fatalf("after a failed renewal Run waits %s with %s to the deadline; want the retry pause, never past the deadline", wait, left)
		}
		clock.Advance(wait)
		if clock.Now().Before(renewedAt.Add(ttl-renew)) && a.Check() != nil {
			t.Fatal("the fencing check fails before the deadline")
		}
	}
	// Renewal attempts at S+10s, then every second to S+14s; the step-down at S+15s.
	if want := 5; attempts != want+1 {
		t.Fatalf("%d steps until the step-down; want %d renewal attempts and the step-down", attempts, want)
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

// TestFailedRenewalIsRetriedBeforeTheDeadline covers one failed renewal with the
// lease timing of the HA integration test (ttl 10s, renew 3s: the deadline is 7s
// after the last renewal). Run used to wait for the deadline after it and step
// down there; it now retries after a pause, keeps the lease, and goes back to the
// renew interval. The standby never takes over.
func TestFailedRenewalIsRetriedBeforeTheDeadline(t *testing.T) {
	const ttl, renew = 10 * time.Second, 3 * time.Second
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
	b.Step(ctx)

	clock.Advance(renew)
	st.setFail(true)
	a.Step(ctx)
	b.Step(ctx)
	if wait := a.untilNextStep(); wait != renew/10 {
		t.Fatalf("after a failed renewal Run waits %s; want the retry pause %s", wait, renew/10)
	}
	// The storage answers again before the deadline: the retry keeps the lease.
	st.setFail(false)
	clock.Advance(renew / 10)
	a.Step(ctx)
	if a.Role() != RoleActive || a.Check() != nil {
		t.Fatal("the active instance lost the lease after one failed renewal")
	}
	if wait := a.untilNextStep(); wait != renew {
		t.Fatalf("after a successful retry Run waits %s; want the renew interval", wait)
	}
	for range 2 * int(ttl/renew) {
		clock.Advance(renew)
		a.Step(ctx)
		b.Step(ctx)
		assertSingleActive(t, a, b)
		if a.Role() != RoleActive || b.Role() != RoleStandby {
			t.Fatalf("roles a %s, b %s; want active and standby", a.Role(), b.Role())
		}
	}
}

// TestRenewTimeoutLeavesTimeForAnotherAttempt checks the bound of one renewal
// request: half the time left to the deadline, so a stalled request leaves room for
// a retry, within half a renew interval (all that is left when less) and a renew
// interval.
func TestRenewTimeoutLeavesTimeForAnotherAttempt(t *testing.T) {
	m := &Manager{cfg: Config{Renew: 3 * time.Second}}
	for _, tc := range []struct{ left, want time.Duration }{
		{30 * time.Second, 3 * time.Second},        // capped at the renew interval
		{4 * time.Second, 2 * time.Second},         // the first renewal of ttl 10s: half of what is left
		{2 * time.Second, 1500 * time.Millisecond}, // at least half a renew interval
		{time.Second, time.Second},                 // the last attempt gets what is left
		{0, 0},
	} {
		if got := m.renewTimeout(tc.left); got != tc.want {
			t.Errorf("renewTimeout(%s) = %s; want %s", tc.left, got, tc.want)
		}
	}
}
