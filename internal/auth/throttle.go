package auth

import (
	"sync"
	"time"
)

// Throttling policy for password attempts. After FreeAttempts failures for an
// (IP, username) pair, each further failure locks the pair out for
// LockoutBase * 2^(failures-FreeAttempts), capped at MaxLockout. A pair's failures are
// forgotten ForgetAfter its last failure once any lockout has expired.
const (
	FreeAttempts   = 5
	LockoutBase    = 30 * time.Second
	MaxLockout     = 15 * time.Minute
	ForgetAfter    = 30 * time.Minute
	maxTrackedKeys = 10000
)

// Per-IP policy. The IP bucket only counts failures; it never refuses a request on its
// own, so the correct password of an unlocked (IP, username) pair always succeeds even
// when many clients share one address (NAT, a reverse proxy without trusted forwarding
// headers). While an IP has ipFreeAttempts or more recent failures, a further failure
// locks its pair after noisyPairBudget failures instead of FreeAttempts, which slows
// password spraying across many usernames.
const (
	ipFreeAttempts  = 4 * FreeAttempts
	noisyPairBudget = 1
)

// Concurrency caps. Attempts are reserved before the (slow) password check, so parallel
// requests cannot all slip past the failure counter. The per-IP cap only applies to
// pairs that already have failures: a first attempt for an (IP, username) pair is
// never refused because other clients behind the same address are busy. CPU spent on
// bcrypt is bounded by the Service's global comparison semaphore instead.
const (
	maxInFlightPerPair = 1
	maxInFlightPerIP   = 4
	// busyRetry is the Retry-After suggested when a concurrency cap is hit.
	busyRetry = time.Second
)

// Setup code policy. The code carries 120 bits of entropy, so throttling is not what
// protects it; wrong codes beyond SetupFreeAttempts per IP within SetupWindow are
// answered with a *ThrottledError to bound log noise, and a correct code is never
// refused.
const (
	SetupFreeAttempts = 100
	SetupWindow       = 15 * time.Minute
)

// Throttle tracks attempts per key in memory. It is safe for concurrent use.
type Throttle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
	now     func() time.Time
}

type throttleEntry struct {
	failures    int
	inFlight    int
	lockedUntil time.Time
	lastFailure time.Time
	// windowStart begins the fixed counting window used by CountFailure.
	windowStart time.Time
}

// NewThrottle returns an empty Throttle using now as its clock.
func NewThrottle(now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{entries: make(map[string]*throttleEntry), now: now}
}

// Attempt is a reserved password attempt. Exactly one of Succeeded, Failed or
// Released must be called; further calls are no-ops.
type Attempt struct {
	t       *Throttle
	pairKey string
	ipKey   string
	once    sync.Once
}

// Reserve atomically checks and reserves an attempt for pairKey (budgeted per pair)
// and ipKey (failures counted per IP; may be empty). It returns a nil Attempt and the
// time to wait when the pair is locked out or a concurrency cap is reached.
func (t *Throttle) Reserve(pairKey, ipKey string) (*Attempt, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)

	pair := t.entryLocked(pairKey)
	forgetStaleLocked(pair, now)
	if wait := pair.lockedUntil.Sub(now); wait > 0 {
		return nil, wait
	}
	if pair.inFlight >= maxInFlightPerPair {
		return nil, busyRetry
	}
	var ip *throttleEntry
	if ipKey != "" {
		ip = t.entryLocked(ipKey)
		forgetStaleLocked(ip, now)
		if pair.failures > 0 && ip.inFlight >= maxInFlightPerIP {
			return nil, busyRetry
		}
		ip.inFlight++
	}
	pair.inFlight++
	return &Attempt{t: t, pairKey: pairKey, ipKey: ipKey}, 0
}

// Succeeded releases the attempt and forgets the pair's failures. The IP's failures
// are kept: one correct login does not whitewash a spraying address.
func (a *Attempt) Succeeded() {
	a.once.Do(func() {
		a.t.mu.Lock()
		defer a.t.mu.Unlock()
		a.t.releaseLocked(a.pairKey, a.ipKey)
		delete(a.t.entries, a.pairKey)
	})
}

// Failed releases the attempt and records a failure for the pair and the IP, locking
// the pair once its budget is exhausted.
func (a *Attempt) Failed() {
	a.once.Do(func() {
		a.t.mu.Lock()
		defer a.t.mu.Unlock()
		now := a.t.now()
		a.t.releaseLocked(a.pairKey, a.ipKey)
		budget := FreeAttempts
		if a.ipKey != "" {
			ip := a.t.entryLocked(a.ipKey)
			ip.failures++
			ip.lastFailure = now
			if ip.failures >= ipFreeAttempts {
				budget = noisyPairBudget
			}
		}
		pair := a.t.entryLocked(a.pairKey)
		pair.failures++
		pair.lastFailure = now
		if over := pair.failures - budget; over >= 0 {
			pair.lockedUntil = now.Add(lockoutFor(over))
		}
	})
}

// Released frees the attempt without counting it either way (e.g. an internal error).
func (a *Attempt) Released() {
	a.once.Do(func() {
		a.t.mu.Lock()
		defer a.t.mu.Unlock()
		a.t.releaseLocked(a.pairKey, a.ipKey)
	})
}

// CountFailure records a failure for key in a fixed window and reports how long the
// caller should wait when more than free failures happened within window (0 while the
// budget lasts).
func (t *Throttle) CountFailure(key string, free int, window time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.pruneLocked(now)
	e := t.entryLocked(key)
	if e.windowStart.IsZero() || now.Sub(e.windowStart) >= window {
		e.windowStart, e.failures = now, 0
	}
	e.failures++
	e.lastFailure = now
	if e.failures <= free {
		return 0
	}
	e.lockedUntil = e.windowStart.Add(window)
	return e.lockedUntil.Sub(now)
}

// releaseLocked decrements the in-flight counters. Caller holds t.mu.
func (t *Throttle) releaseLocked(pairKey, ipKey string) {
	if e, ok := t.entries[pairKey]; ok && e.inFlight > 0 {
		e.inFlight--
	}
	if ipKey != "" {
		if e, ok := t.entries[ipKey]; ok && e.inFlight > 0 {
			e.inFlight--
		}
	}
}

// entryLocked returns the entry for key, creating it. Caller holds t.mu.
func (t *Throttle) entryLocked(key string) *throttleEntry {
	e, ok := t.entries[key]
	if !ok {
		e = &throttleEntry{}
		t.entries[key] = e
	}
	return e
}

// forgetStaleLocked clears the failures of an entry whose lockout has expired and whose
// last failure is older than ForgetAfter. Caller holds t.mu.
func forgetStaleLocked(e *throttleEntry, now time.Time) {
	if e.failures > 0 && !now.Before(e.lockedUntil) && now.Sub(e.lastFailure) > ForgetAfter {
		e.failures = 0
	}
}

// lockoutFor returns the lockout after over failures beyond the budget.
func lockoutFor(over int) time.Duration {
	if over >= 16 { // 30s << 16 is far above the cap
		return MaxLockout
	}
	return min(LockoutBase<<over, MaxLockout)
}

// pruneLocked drops stale entries and bounds memory. Caller holds t.mu.
func (t *Throttle) pruneLocked(now time.Time) {
	if len(t.entries) < maxTrackedKeys/2 {
		return
	}
	for k, e := range t.entries {
		if e.inFlight == 0 && now.After(e.lockedUntil) && now.Sub(e.lastFailure) > ForgetAfter {
			delete(t.entries, k)
		}
	}
	if len(t.entries) < maxTrackedKeys {
		return
	}
	// Still full of recent entries: drop idle unlocked ones, keeping active lockouts.
	for k, e := range t.entries {
		if e.inFlight == 0 && now.After(e.lockedUntil) {
			delete(t.entries, k)
		}
		if len(t.entries) < maxTrackedKeys/2 {
			return
		}
	}
}
