package notify_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// Channel secrets that must never reach the outbox.
const (
	hookPathToken = "T000/B000/outbox-path-token-1"
	hookSecret    = "outbox-hmac-secret-2"
	hookHeader    = "Bearer outbox-header-token-3"
)

// recorder is a Notifier that records the messages it accepts and fails the
// attempts fail says to fail.
type recorder struct {
	mu   sync.Mutex
	got  []notify.Message
	fail func(msg notify.Message, attempt int) error
	seen map[string]int
}

func (r *recorder) Type() string { return "webhook" }

func (r *recorder) Send(_ context.Context, msg notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[msg.Event.Detail]++
	if r.fail != nil {
		if err := r.fail(msg, r.seen[msg.Event.Detail]); err != nil {
			return err
		}
	}
	r.got = append(r.got, msg)
	return nil
}

func (r *recorder) messages() []notify.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Message(nil), r.got...)
}

// outcomes counts observed delivery outcomes.
type outcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *outcomes) observe(_ notify.ChannelType, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n == nil {
		o.n = map[string]int{}
	}
	o.n[outcome]++
}

func (o *outcomes) count(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n[outcome]
}

// seedChannel stores a webhook channel with secrets and a rule sending
// security.key_rotated to it.
func seedChannel(t *testing.T, s *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	ch := &notify.Channel{ID: "ch_hook", Name: "Hook", Type: notify.ChannelWebhook, Enabled: true,
		Webhook: &notify.WebhookConfig{URL: "https://hooks.example.com/" + hookPathToken, Secret: hookSecret,
			Headers: map[string]string{"Authorization": hookHeader}}}
	if err := s.SaveChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	rule := &notify.Rule{ID: "rule_keys", Name: "Keys", Enabled: true, Events: []events.EventType{events.SecurityKeyRotated}, ChannelIDs: []string{"ch_hook"}}
	if err := s.SaveRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
}

func keyRotated(detail string) events.Event {
	return events.Event{Type: events.SecurityKeyRotated, Time: time.Now().UTC(), Action: "secret_key", Actor: "alice", Detail: detail}
}

// runService runs svc until the returned stop function is called.
func runService(t *testing.T, svc *notify.Service) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := svc.Run(ctx); err != nil {
			t.Error(err)
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func heads(t *testing.T, s *store.SQLiteStore) []notify.OutboxEntry {
	t.Helper()
	list, err := s.OutboxHeads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A delivery queued before the process died (the service never ran, the
// database was closed) is sent after the restart, with the event's ID, and
// removed from the outbox; the outbox rows hold no channel secret.
func TestOutboxSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	box := storetest.NewBox(t)
	first := storetest.OpenWithBox(t, path, box)
	seedChannel(t, first)
	dead := &recorder{}
	svc := notify.NewService(first, notify.WithOutbox(first),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return dead, nil }))
	svc.HandleEvent(context.Background(), keyRotated("rotation 1"))
	// The process dies before any delivery: nothing was sent, the row is stored.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if len(dead.messages()) != 0 {
		t.Fatal("a delivery was sent without Run")
	}

	// No secret of the channel is stored with the delivery.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	var dump strings.Builder
	r, err := raw.Query("SELECT channel_id, channel_type, event, last_error FROM notification_outbox")
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var a, b, c, d string
		if err = r.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		rows++
		fmt.Fprintln(&dump, a, b, c, d)
	}
	_ = r.Close()
	_ = raw.Close()
	if rows != 1 {
		t.Fatalf("%d outbox rows, want 1", rows)
	}
	for _, secret := range []string{hookPathToken, hookSecret, hookHeader, "hooks.example.com"} {
		if strings.Contains(dump.String(), secret) {
			t.Errorf("the outbox row holds %q: %s", secret, dump.String())
		}
	}

	second := storetest.OpenWithBox(t, path, box)
	rec := &recorder{}
	svc = notify.NewService(second, notify.WithOutbox(second),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return rec, nil }))
	stop := runService(t, svc)
	defer stop()
	waitFor(t, "the delivery after the restart", func() bool { return len(rec.messages()) == 1 })
	msg := rec.messages()[0]
	if msg.Event.Type != events.SecurityKeyRotated || msg.Event.Detail != "rotation 1" || !strings.HasPrefix(msg.Event.ID, "evt_") {
		t.Fatalf("delivered %+v", msg.Event)
	}
	if !strings.Contains(msg.Subject, "Key rotated") {
		t.Errorf("subject %q not rendered at send time", msg.Subject)
	}
	if p := notify.NewWebhookPayload(msg); p.EventID != msg.Event.ID {
		t.Errorf("webhook event_id %q, want %q", p.EventID, msg.Event.ID)
	}
	waitFor(t, "the outbox to empty", func() bool { return len(heads(t, second)) == 0 })
}

// The outbox is capped: the oldest deliveries are dropped and counted.
func TestOutboxCap(t *testing.T) {
	s := storetest.New(t)
	seedChannel(t, s)
	obs := &outcomes{}
	svc := notify.NewService(s, notify.WithOutbox(s), notify.WithOutboxLimit(3), notify.WithObserver(obs.observe),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return &recorder{}, nil }))
	for i := range 5 {
		svc.HandleEvent(context.Background(), keyRotated(fmt.Sprintf("rotation %d", i)))
	}
	if got := obs.count(notify.OutcomeDropped); got != 2 {
		t.Fatalf("dropped %d, want 2", got)
	}
	// One channel: its head is the oldest kept delivery.
	h := heads(t, s)
	if len(h) != 1 || h[0].Event.Detail != "rotation 2" {
		t.Fatalf("heads %+v, want rotation 2 first", h)
	}
	rec := &recorder{}
	svc = notify.NewService(s, notify.WithOutbox(s),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return rec, nil }))
	stop := runService(t, svc)
	defer stop()
	waitFor(t, "the kept deliveries", func() bool { return len(rec.messages()) == 3 })
	for i, m := range rec.messages() {
		if want := fmt.Sprintf("rotation %d", i+2); m.Event.Detail != want {
			t.Errorf("delivery %d = %q, want %q (in order)", i, m.Event.Detail, want)
		}
	}
}

// A failing delivery is retried with backoff and the later deliveries of its
// channel wait for it; a permanent failure is given up and counted once.
func TestOutboxRetriesInOrder(t *testing.T) {
	s := storetest.New(t)
	seedChannel(t, s)
	rec := &recorder{fail: func(msg notify.Message, attempt int) error {
		switch {
		case msg.Event.Detail == "first" && attempt == 1:
			return errors.New("webhook: status 503")
		case msg.Event.Detail == "doomed":
			return fmt.Errorf("webhook: status 400: %w", notify.ErrPermanent)
		}
		return nil
	}}
	obs := &outcomes{}
	svc := notify.NewService(s, notify.WithOutbox(s), notify.WithObserver(obs.observe),
		notify.WithRetryPolicy(notify.RetryPolicy{Retries: 0, AttemptTimeout: time.Second}),
		notify.WithOutboxRetry(5, 20*time.Millisecond, 40*time.Millisecond),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return rec, nil }))
	stop := runService(t, svc)
	defer stop()
	ctx := context.Background()
	svc.HandleEvent(ctx, keyRotated("first"))
	svc.HandleEvent(ctx, keyRotated("doomed"))
	svc.HandleEvent(ctx, keyRotated("second"))
	waitFor(t, "both deliveries", func() bool { return len(rec.messages()) == 2 })
	got := rec.messages()
	if got[0].Event.Detail != "first" || got[1].Event.Detail != "second" {
		t.Fatalf("delivered %q then %q; want first then second", got[0].Event.Detail, got[1].Event.Detail)
	}
	waitFor(t, "the outbox to empty", func() bool { return len(heads(t, s)) == 0 })
	if f := obs.count(notify.OutcomeFailure); f != 1 {
		t.Errorf("failures counted %d, want 1 (the permanent one)", f)
	}
	if ok := obs.count(notify.OutcomeSuccess); ok != 2 {
		t.Errorf("successes counted %d, want 2", ok)
	}
}

// Deliveries for a channel deleted meanwhile are dropped from the outbox.
func TestOutboxDropsDeletedChannels(t *testing.T) {
	s := storetest.New(t)
	seedChannel(t, s)
	svc := notify.NewService(s, notify.WithOutbox(s),
		notify.WithNotifierFactory(func(*notify.Channel) (notify.Notifier, error) { return &recorder{}, nil }))
	svc.HandleEvent(context.Background(), keyRotated("orphan"))
	if err := s.DeleteChannel(context.Background(), "ch_hook"); err != nil {
		t.Fatal(err)
	}
	stop := runService(t, svc)
	defer stop()
	waitFor(t, "the outbox to empty", func() bool { return len(heads(t, s)) == 0 })
}
