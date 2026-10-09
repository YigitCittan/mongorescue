package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// errLocked is a failing outbox write.
var errLocked = errors.New("database is locked")

// memOutbox is an in-memory Outbox whose deletes and reschedules fail while
// their counters are positive, counting every call.
type memOutbox struct {
	mu          sync.Mutex
	rows        []OutboxEntry
	nextID      int64
	failDeletes int
	failRetries int
	deletes     int
	retries     int
}

func (m *memOutbox) EnqueueDeliveries(_ context.Context, entries []OutboxEntry, _ int) ([]OutboxEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		m.nextID++
		e.ID = m.nextID
		m.rows = append(m.rows, e)
	}
	return nil, nil
}

func (m *memOutbox) OutboxHeads(context.Context) ([]OutboxEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []OutboxEntry
	for _, r := range m.rows {
		if !seen[r.ChannelID] {
			seen[r.ChannelID] = true
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memOutbox) DeleteDelivery(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	if m.failDeletes > 0 {
		m.failDeletes--
		return errLocked
	}
	m.rows = slices.DeleteFunc(m.rows, func(r OutboxEntry) bool { return r.ID == id })
	return nil
}

func (m *memOutbox) RetryDelivery(_ context.Context, id int64, attempts int, next time.Time, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries++
	if m.failRetries > 0 {
		m.failRetries--
		return errLocked
	}
	for i := range m.rows {
		if m.rows[i].ID == id {
			m.rows[i].Attempts, m.rows[i].NextAttemptAt = attempts, next
		}
	}
	return nil
}

func (m *memOutbox) counts() (rows, deletes, retries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), m.deletes, m.retries
}

func outboxService(t *testing.T, ob *memOutbox, fake *fakeNotifier) (*Service, func()) {
	t.Helper()
	repo := newMemRepo()
	_ = repo.SaveChannel(context.Background(), &Channel{ID: "ch1", Name: "one", Type: ChannelWebhook, Enabled: true,
		Webhook: &WebhookConfig{URL: "https://hooks.example.com/x"}})
	_ = repo.SaveRule(context.Background(), &Rule{ID: "r1", Name: "r", Enabled: true,
		Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"ch1"}})
	svc := NewService(repo, WithOutbox(ob),
		WithRetryPolicy(RetryPolicy{Retries: 0, AttemptTimeout: time.Second}),
		WithOutboxRetry(5, 30*time.Millisecond, 60*time.Millisecond),
		WithNotifierFactory(func(*Channel) (Notifier, error) { return fake, nil }))
	svc.heldBackoff, svc.heldMaxBackoff = 50*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = svc.Run(ctx) })
	return svc, func() {
		cancel()
		wg.Wait()
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A delivered notification whose removal fails is never sent again: only the
// removal is retried, with backoff, and the dispatcher does not spin.
func TestOutboxFailedRemovalIsNotResent(t *testing.T) {
	ob := &memOutbox{failDeletes: 3}
	fake := &fakeNotifier{}
	svc, stop := outboxService(t, ob, fake)
	defer stop()
	svc.HandleEvent(context.Background(), events.Event{Type: events.BackupFailed, Database: "shop"})
	waitUntil(t, "the removal", func() bool { rows, _, _ := ob.counts(); return rows == 0 })
	if n := fake.count(); n != 1 {
		t.Fatalf("sent %d times, want 1", n)
	}
	// Three failures, backoff 50, 100, 100 ms: four deletes, not hundreds.
	time.Sleep(100 * time.Millisecond)
	if _, deletes, _ := ob.counts(); deletes != 4 {
		t.Fatalf("%d delete calls, want 4 (no hot loop)", deletes)
	}
	svc.flightMu.Lock()
	held := len(svc.held)
	svc.flightMu.Unlock()
	if held != 0 {
		t.Fatalf("%d held states left", held)
	}
}

// brokenRepo cannot load its channels (a decryption failure).
type brokenRepo struct{ *memRepo }

func (brokenRepo) GetChannel(context.Context, string) (*Channel, error) {
	return nil, errors.New("store: decrypt channel ch1: secretbox: message authentication failed")
}

// A channel that cannot be loaded counts rounds like a failed send, is given up
// after the round limit (so it never blocks the channel for good) and is named
// in the settings warning.
func TestOutboxUnreadableChannelIsGivenUp(t *testing.T) {
	repo := newMemRepo()
	_ = repo.SaveChannel(context.Background(), &Channel{ID: "ch1", Name: "one", Type: ChannelWebhook, Enabled: true,
		Webhook: &WebhookConfig{URL: "https://hooks.example.com/x"}})
	_ = repo.SaveRule(context.Background(), &Rule{ID: "r1", Name: "r", Enabled: true,
		Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"ch1"}})
	ob := &memOutbox{}
	fake := &fakeNotifier{}
	var mu sync.Mutex
	warned := map[string]string{}
	failures := 0
	svc := NewService(brokenRepo{repo}, WithOutbox(ob),
		WithOutboxRetry(3, 10*time.Millisecond, 20*time.Millisecond),
		WithChannelWarning(func(id, problem string) {
			mu.Lock()
			defer mu.Unlock()
			warned[id] = problem
		}),
		WithObserver(func(_ ChannelType, outcome string) {
			if outcome == OutcomeFailure {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}),
		WithNotifierFactory(func(*Channel) (Notifier, error) { return fake, nil }))
	_, _ = ob.EnqueueDeliveries(context.Background(), []OutboxEntry{{ChannelID: "ch1", ChannelType: ChannelWebhook,
		Event: events.Event{Type: events.BackupFailed, ID: "evt_1"}, NextAttemptAt: time.Now()}}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = svc.Run(ctx) })
	defer func() {
		cancel()
		wg.Wait()
	}()
	waitUntil(t, "the delivery to be given up", func() bool { rows, _, _ := ob.counts(); return rows == 0 })
	_, _, retries := ob.counts()
	mu.Lock()
	defer mu.Unlock()
	if retries != 2 || failures != 1 || fake.count() != 0 {
		t.Fatalf("%d reschedules, %d failures, %d sent; want 2, 1, 0", retries, failures, fake.count())
	}
	if p := warned["ch1"]; p == "" || !strings.Contains(p, "message authentication failed") {
		t.Fatalf("warning for ch1 = %q", p)
	}
}

// A failed reschedule keeps the next attempt in memory: the delivery waits for
// its backoff instead of being retried at once, and succeeds later.
func TestOutboxFailedRescheduleKeepsBackoff(t *testing.T) {
	ob := &memOutbox{failRetries: 1}
	fake := &fakeNotifier{failures: 1, err: errors.New("webhook: status 503")}
	var sends []time.Time
	var mu sync.Mutex
	fake.onSend = func() {
		mu.Lock()
		sends = append(sends, time.Now())
		mu.Unlock()
	}
	svc, stop := outboxService(t, ob, fake)
	defer stop()
	svc.HandleEvent(context.Background(), events.Event{Type: events.BackupFailed, Database: "shop"})
	waitUntil(t, "the delivery", func() bool { rows, _, _ := ob.counts(); return rows == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 2 || fake.count() != 1 {
		t.Fatalf("%d attempts, %d delivered; want 2 and 1", len(sends), fake.count())
	}
	if gap := sends[1].Sub(sends[0]); gap < 25*time.Millisecond {
		t.Fatalf("retried after %s; the backoff of the unsaved reschedule was lost", gap)
	}
	if _, _, retries := ob.counts(); retries != 1 {
		t.Fatalf("%d reschedule writes, want 1", retries)
	}
}
