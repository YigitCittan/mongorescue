package ha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

const (
	testTTL   = 30 * time.Second
	testRenew = 10 * time.Second
)

// fakeClock is a manual monotonic clock shared by the managers of a test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memStore is an in-memory storage.LeaseStore. fail makes every call fail with an
// I/O error; beforeWrite runs before a write is applied, afterWrite after.
type memStore struct {
	mu          sync.Mutex
	data        map[string][]byte
	fail        bool
	beforeRead  func()
	beforeWrite func()
	afterWrite  func()
}

func newMemStore() *memStore { return &memStore{data: map[string][]byte{}} }

var errIO = errors.New("connection reset")

func memVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (s *memStore) ReadVersioned(_ context.Context, key string) ([]byte, string, error) {
	s.mu.Lock()
	hook := s.beforeRead
	s.beforeRead = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, "", errIO
	}
	d, ok := s.data[key]
	if !ok {
		return nil, "", storage.ErrNotFound
	}
	return d, memVersion(d), nil
}

func (s *memStore) write(key string, data []byte, check func(cur []byte, ok bool) bool) (string, error) {
	s.mu.Lock()
	hook := s.beforeWrite
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	s.mu.Lock()
	if s.fail {
		s.mu.Unlock()
		return "", errIO
	}
	cur, ok := s.data[key]
	if !check(cur, ok) {
		s.mu.Unlock()
		return "", storage.ErrPreconditionFailed
	}
	s.data[key] = data
	after := s.afterWrite
	s.mu.Unlock()
	if after != nil {
		after()
	}
	return memVersion(data), nil
}

func (s *memStore) CreateIfAbsent(_ context.Context, key string, data []byte) (string, error) {
	return s.write(key, data, func(_ []byte, ok bool) bool { return !ok })
}

func (s *memStore) ReplaceIfMatch(_ context.Context, key string, data []byte, version string) (string, error) {
	return s.write(key, data, func(cur []byte, ok bool) bool { return ok && memVersion(cur) == version })
}

func (s *memStore) setFail(f bool) {
	s.mu.Lock()
	s.fail = f
	s.mu.Unlock()
}

func (s *memStore) lease(t *testing.T) Lease {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := DecodeLease(s.data[LeaseKey("prod")])
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func newTestManager(t *testing.T, st storage.LeaseStore, clock *fakeClock, id string) *Manager {
	t.Helper()
	m, err := New(Config{
		Store: st, Cluster: "prod", InstanceID: id, InstanceName: "host-" + id, KeyID: "key1",
		TTL: testTTL, Renew: testRenew, Now: clock.Now, Wall: clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// assertSingleActive fails when both managers pass the fencing check.
func assertSingleActive(t *testing.T, ms ...*Manager) {
	t.Helper()
	active := 0
	for _, m := range ms {
		if m.Check() == nil {
			active++
		}
	}
	if active > 1 {
		t.Fatalf("%d instances pass the fencing check at once", active)
	}
}

func TestValidateTiming(t *testing.T) {
	for _, tc := range []struct {
		ttl, renew time.Duration
		ok         bool
	}{
		{30 * time.Second, 10 * time.Second, true},
		{30 * time.Second, 15 * time.Second, false},
		{30 * time.Second, 500 * time.Millisecond, false},
		{4 * time.Second, time.Second, false},
		{11 * time.Minute, time.Minute, false},
		{5 * time.Second, 2 * time.Second, true},
	} {
		if err := ValidateTiming(tc.ttl, tc.renew); (err == nil) != tc.ok {
			t.Errorf("ValidateTiming(%s, %s) = %v; want ok=%v", tc.ttl, tc.renew, err, tc.ok)
		}
	}
	if err := ValidateCluster("Prod"); err == nil {
		t.Error("ValidateCluster accepted an upper-case name")
	}
	if err := ValidateCluster("prod-eu.1"); err != nil {
		t.Errorf("ValidateCluster: %v", err)
	}
}

func TestFirstInstanceCreatesTheLease(t *testing.T) {
	clock, st := newFakeClock(), newMemStore()
	a := newTestManager(t, st, clock, "a")
	if a.Check() == nil {
		t.Fatal("a standby passed the fencing check")
	}
	a.Step(context.Background())
	if a.Role() != RoleActive || a.Epoch() != 1 || a.Check() != nil {
		t.Fatalf("after the first step: role %s epoch %d check %v; want active, epoch 1", a.Role(), a.Epoch(), a.Check())
	}
	select {
	case <-a.Changes():
	default:
		t.Fatal("no change was signalled")
	}
	l := st.lease(t)
	if l.Holder != "a" || l.Epoch != 1 || l.KeyID != "key1" || l.TTLSeconds != 30 {
		t.Fatalf("lease = %+v", l)
	}
}

func TestStandbyTakesOverWithinTTLPlusRenew(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a, b := newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b")
	a.Step(ctx)
	b.Step(ctx)
	// While a renews, b stays standby however long it waits.
	for range 20 {
		clock.Advance(testRenew)
		a.Step(ctx)
		b.Step(ctx)
		assertSingleActive(t, a, b)
		if b.Role() != RoleStandby {
			t.Fatal("the standby took over a lease that is being renewed")
		}
	}
	// a dies right after its last renewal; b polls every renew interval.
	lastRenew := clock.Now()
	for b.Role() != RoleActive {
		clock.Advance(testRenew)
		b.Step(ctx)
		assertSingleActive(t, a, b)
		if clock.Now().Sub(lastRenew) > testTTL+testRenew {
			t.Fatalf("no takeover %s after the last renewal", clock.Now().Sub(lastRenew))
		}
	}
	if took := clock.Now().Sub(lastRenew); took < testTTL {
		t.Fatalf("took over %s after the last renewal; must wait at least the TTL", took)
	}
	if b.Epoch() != 2 || st.lease(t).Holder != "b" {
		t.Fatalf("b holds epoch %d, lease %+v; want epoch 2 held by b", b.Epoch(), st.lease(t))
	}
	// a wakes up: its fencing check failed long ago and its renewal fails.
	if a.Check() == nil {
		t.Fatal("the old active passes the fencing check after the takeover")
	}
	a.Step(ctx)
	if a.Role() != RoleStandby {
		t.Fatal("the old active did not step down")
	}
}

func TestDelayedRenewalAfterTakeoverLoses(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a, b := newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b")
	a.Step(ctx)
	b.Step(ctx)
	// a sends a renewal that hangs in the network; meanwhile time passes, b sees
	// the lease unchanged for the TTL and takes over. Only then does the renewal
	// reach the storage.
	st.beforeWrite = func() {
		st.beforeWrite = nil
		for range 4 {
			clock.Advance(testRenew)
			b.Step(ctx)
		}
		if b.Role() != RoleActive {
			t.Error("b did not take over while a's renewal hung")
		}
		if a.Check() == nil {
			t.Error("split brain: a passes the fencing check while b is active")
		}
	}
	clock.Advance(testRenew)
	a.Step(ctx)
	if a.Role() != RoleStandby {
		t.Fatal("a kept the active role after its delayed renewal lost")
	}
	if l := st.lease(t); l.Holder != "b" || l.Epoch != 2 {
		t.Fatalf("lease = %+v; want b's epoch 2", l)
	}
	assertSingleActive(t, a, b)
}

func TestDelayedRenewalBeforeTakeoverWins(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a, b := newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b")
	a.Step(ctx)
	b.Step(ctx)
	// The renewal commits, but its response is lost for long: b's takeover must
	// fail on the new version, and a must step down once its deadline passes.
	st.afterWrite = func() {
		st.afterWrite = nil
		for range 4 {
			clock.Advance(testRenew)
			b.Step(ctx)
			assertSingleActive(t, a, b)
		}
	}
	clock.Advance(testRenew)
	a.Step(ctx)
	// a's write committed when it was sent, so b first saw it during the delay
	// and has not seen it unchanged for a TTL yet.
	if b.Role() == RoleActive && a.Check() == nil {
		t.Fatal("split brain after a delayed renewal")
	}
	for range 10 {
		clock.Advance(testRenew)
		a.Step(ctx)
		b.Step(ctx)
		assertSingleActive(t, a, b)
	}
}

func TestStepDownWhenTheStorageIsUnreachable(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a := newTestManager(t, st, clock, "a")
	a.Step(ctx)
	lastRenew := clock.Now()
	st.setFail(true)
	clock.Advance(testRenew)
	a.Step(ctx)
	if a.Role() != RoleActive || a.Check() != nil {
		t.Fatal("one failed renewal must not step down yet")
	}
	if a.Status().LeaseHealthy || a.Status().LeaseErrors != 1 {
		t.Fatalf("status = %+v; want an unhealthy lease and one error", a.Status())
	}
	// The fencing check fails at ttl - renew after the last renewal, even before
	// the next step runs.
	clock.Advance(testTTL - 2*testRenew)
	if clock.Now().Sub(lastRenew) != testTTL-testRenew {
		t.Fatal("test arithmetic")
	}
	if a.Check() == nil {
		t.Fatal("the fencing check passes past the step-down deadline")
	}
	a.Step(ctx)
	if a.Role() != RoleStandby {
		t.Fatal("did not step down past the deadline")
	}
	select {
	case <-a.Changes():
	default:
		t.Fatal("no change was signalled")
	}
}

func TestConflictingWriteStepsDownAtOnce(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a := newTestManager(t, st, clock, "a")
	a.Step(ctx)
	// Someone deletes the lease by hand.
	st.mu.Lock()
	delete(st.data, LeaseKey("prod"))
	st.mu.Unlock()
	clock.Advance(testRenew)
	a.Step(ctx)
	if a.Role() != RoleStandby || a.Check() == nil {
		t.Fatal("a renewal that found no lease did not step down")
	}
	// Another instance creates a new lease with a higher epoch than any it saw.
	b := newTestManager(t, st, clock, "b")
	b.mu.Lock()
	b.seenEpoch = 1
	b.mu.Unlock()
	b.Step(ctx)
	if b.Epoch() != 2 {
		t.Fatalf("b epoch = %d; want 2", b.Epoch())
	}
	// a waits out its cooldown and does not take b's lease.
	for range 6 {
		clock.Advance(testRenew)
		b.Step(ctx)
		a.Step(ctx)
		assertSingleActive(t, a, b)
	}
	if a.Role() != RoleStandby || b.Role() != RoleActive {
		t.Fatalf("roles a=%s b=%s", a.Role(), b.Role())
	}
}

func TestReleaseHandsOverAtTheNextRead(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a, b := newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b")
	a.Step(ctx)
	b.Step(ctx)
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Role() != RoleStandby || !st.lease(t).Released() {
		t.Fatalf("after Release: role %s, lease %+v", a.Role(), st.lease(t))
	}
	clock.Advance(testRenew)
	a.Step(ctx) // in its cooldown: it does not grab the released lease back
	if a.Role() != RoleStandby {
		t.Fatal("the releasing instance took the lease back")
	}
	b.Step(ctx)
	if b.Role() != RoleActive || b.Epoch() != 2 {
		t.Fatalf("b role %s epoch %d; want active with epoch 2", b.Role(), b.Epoch())
	}
	if err := a.Release(ctx); err != nil {
		t.Fatalf("Release of a standby = %v", err)
	}
}

func TestStepDownWithRelease(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a := newTestManager(t, st, clock, "a")
	a.Step(ctx)
	if err := a.StepDown(ctx, "no usable snapshot", true); err != nil {
		t.Fatal(err)
	}
	if a.Role() != RoleStandby || !st.lease(t).Released() {
		t.Fatal("StepDown did not release")
	}
	// Alone in the cluster, it acquires again after its cooldown.
	clock.Advance(testRenew)
	a.Step(ctx)
	if a.Role() != RoleStandby {
		t.Fatal("acquired during the cooldown")
	}
	clock.Advance(testTTL)
	a.Step(ctx)
	if a.Role() != RoleActive || a.Epoch() != 2 {
		t.Fatalf("role %s epoch %d after the cooldown; want active with epoch 2", a.Role(), a.Epoch())
	}
}

func TestWallClockSkewDoesNotMatter(t *testing.T) {
	ctx := context.Background()
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		clock, st := newFakeClock(), newMemStore()
		a := newTestManager(t, st, clock, "a")
		a.cfg.Wall = func() time.Time { return clock.Now().Add(skew) }
		b := newTestManager(t, st, clock, "b")
		a.Step(ctx)
		b.Step(ctx)
		for range 5 {
			clock.Advance(testRenew)
			a.Step(ctx)
			b.Step(ctx)
		}
		if b.Role() != RoleStandby {
			t.Fatalf("skew %s: the standby took over a renewed lease", skew)
		}
		lastRenew := clock.Now()
		for b.Role() != RoleActive {
			clock.Advance(time.Second)
			b.Step(ctx)
		}
		if took := clock.Now().Sub(lastRenew); took < testTTL || took > testTTL+testRenew {
			t.Fatalf("skew %s: took over %s after the last renewal", skew, took)
		}
	}
}

func TestKeyMismatchNeverTakesOver(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	a := newTestManager(t, st, clock, "a")
	a.Step(ctx)
	b := newTestManager(t, st, clock, "b")
	b.cfg.KeyID = "other"
	if _, err := b.Observe(ctx); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Observe = %v; want ErrKeyMismatch", err)
	}
	for range 10 {
		clock.Advance(testRenew)
		b.Step(ctx)
	}
	if b.Role() != RoleStandby || !b.Status().KeyMismatch {
		t.Fatalf("status = %+v; want a standby reporting the key mismatch", b.Status())
	}
}

func TestUnreadableLeaseExpiresLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	st.data[LeaseKey("prod")] = []byte("{not json")
	b := newTestManager(t, st, clock, "b")
	b.Step(ctx)
	if b.Role() != RoleStandby {
		t.Fatal("an unreadable lease was taken at once")
	}
	clock.Advance(testTTL)
	b.Step(ctx)
	if b.Role() != RoleActive {
		t.Fatal("an unreadable lease did not expire")
	}
}

// TestFlappingStorageNeverHasTwoActives runs three instances against a storage that
// fails at random, and checks the invariants on every tick: at most one instance
// passes the fencing check, and the epochs of successive holders grow.
func TestFlappingStorageNeverHasTwoActives(t *testing.T) {
	ctx := context.Background()
	clock, st := newFakeClock(), newMemStore()
	ms := []*Manager{newTestManager(t, st, clock, "a"), newTestManager(t, st, clock, "b"), newTestManager(t, st, clock, "c")}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded, reproducible test schedule.
	var lastEpoch uint64
	holders := map[uint64]string{}
	for tick := range 3000 {
		st.setFail(rng.IntN(100) < 30)
		clock.Advance(time.Duration(1+rng.IntN(4)) * time.Second)
		m := ms[rng.IntN(len(ms))]
		m.Step(ctx)
		assertSingleActive(t, ms...)
		for _, x := range ms {
			// An instance past its deadline may not have stepped down yet; only one
			// that passes the fencing check counts as active.
			if e := x.Epoch(); e != 0 && x.Check() == nil {
				if e < lastEpoch {
					t.Fatalf("tick %d: an active instance holds epoch %d after epoch %d", tick, e, lastEpoch)
				}
				if h, ok := holders[e]; ok && h != x.cfg.InstanceID {
					t.Fatalf("tick %d: epoch %d held by %s and %s", tick, e, h, x.cfg.InstanceID)
				}
				holders[e], lastEpoch = x.cfg.InstanceID, e
			}
		}
	}
	if lastEpoch < 3 {
		t.Fatalf("only %d epochs in 3000 flapping ticks; the test does not exercise takeovers", lastEpoch)
	}
	// Once the storage is stable, one instance ends up active.
	st.setFail(false)
	for range 20 {
		clock.Advance(testRenew)
		for _, m := range ms {
			m.Step(ctx)
		}
		assertSingleActive(t, ms...)
	}
	active := 0
	for _, m := range ms {
		if m.Check() == nil {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d active instances on a stable storage; want 1", active)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	st := newMemStore()
	m, err := New(Config{Store: st, Cluster: "prod", InstanceID: "a", TTL: MinTTL, Renew: MinRenew,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	select {
	case <-m.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not acquire the free lease")
	}
	if m.Check() != nil {
		t.Fatal("not active after Run acquired the lease")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if err = m.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Cluster: "prod", InstanceID: "a", TTL: testTTL, Renew: testRenew}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New without a store = %v", err)
	}
	if _, err := New(Config{Store: newMemStore(), Cluster: "prod", InstanceID: "a", TTL: testTTL, Renew: testTTL}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New with renew = ttl: %v", err)
	}
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	st, err := LoadState(dir)
	if err != nil || len(st.InstanceID) != 16 {
		t.Fatalf("LoadState = %+v, %v", st, err)
	}
	st.Cluster, st.LastActiveEpoch = "prod", 7
	if err = SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	again, err := LoadState(dir)
	if err != nil || again.InstanceID != st.InstanceID || again.LastActiveEpoch != 7 {
		t.Fatalf("LoadState = %+v, %v", again, err)
	}
}
