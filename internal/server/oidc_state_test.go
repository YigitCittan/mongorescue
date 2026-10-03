package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
)

// TestUsedStatesAreBoundedAndNeverRefuseAFreshState: the used-state set forgets
// expired flows, evicts the oldest flow when full, and so never refuses a valid
// fresh state however many states an attacker gets recorded; a replay of a
// remembered state is still refused.
func TestUsedStatesAreBoundedAndNeverRefuseAFreshState(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	u := newStateSetOf(100)
	if !u.consume("first", now, now) || u.consume("first", now, now) {
		t.Fatal("a state must be accepted once")
	}
	// Flood the set with fresh states: it stays bounded and keeps accepting.
	for i := range 1000 {
		start := now.Add(time.Duration(i) * time.Millisecond)
		if !u.consume(fmt.Sprintf("flood-%d", i), start, start) {
			t.Fatalf("fresh state %d refused", i)
		}
		if u.size() > 100 {
			t.Fatalf("the set grew to %d entries", u.size())
		}
	}
	at := now.Add(2 * time.Second)
	if !u.consume("fresh", at, at) {
		t.Fatal("a fresh state was refused after the flood")
	}
	if u.consume("fresh", at, at) || u.consume("flood-999", at, at) {
		t.Fatal("a remembered state was accepted twice")
	}
	// The oldest flows were evicted first.
	if !u.consume("flood-0", now, at) {
		t.Fatal("the oldest flow should have been evicted")
	}
	// Expired flows are forgotten by their start, not by when they were consumed.
	later := now.Add(oidcFlowLifetime + oidc.Leeway + time.Minute)
	if !u.consume("after-expiry", later, later) || u.size() != 1 {
		t.Fatalf("expired flows kept: %d entries", u.size())
	}
}
