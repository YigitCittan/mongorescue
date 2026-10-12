package ha

import (
	"context"
	"testing"
	"time"
)

// TestSlowReadOfARenewalDoesNotShortenTheExpiry covers a standby read that is in
// flight longer than the renew interval and returns a renewal committed meanwhile:
// the standby measures the expiry from when the read returned, so it never takes
// over before the active instance's deadline plus the renew margin.
func TestSlowReadOfARenewalDoesNotShortenTheExpiry(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a, b := newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b")
	a.Step(ctx)
	clock.Advance(time.Second)

	var renewedAt time.Time
	st.beforeRead = func() {
		// The read hangs for longer than a renew interval; meanwhile a renews and
		// the read returns that renewal.
		clock.Advance(testRenew + 5*time.Second)
		renewedAt = clock.Now()
		a.Step(ctx)
		if a.Role() != RoleActive {
			t.Error("a failed to renew")
		}
	}
	b.Step(ctx)
	if renewedAt.IsZero() {
		t.Fatal("the slow read did not run")
	}

	// a dies right after that renewal; b polls every second.
	activeDeadline := renewedAt.Add(testTTL - testRenew)
	for b.Role() != RoleActive {
		clock.Advance(time.Second)
		b.Step(ctx)
		assertSingleActive(t, a, b)
		if clock.Now().Sub(renewedAt) > 2*testTTL {
			t.Fatal("b never took over")
		}
	}
	if took := clock.Now(); took.Before(activeDeadline.Add(testRenew)) {
		t.Fatalf("b took over %s after a's renewal; a works until %s after it, so the takeover must wait until at least %s",
			took.Sub(renewedAt), activeDeadline.Sub(renewedAt), activeDeadline.Add(testRenew).Sub(renewedAt))
	}
}
