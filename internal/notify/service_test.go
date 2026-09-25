package notify

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// memRepo is an in-memory Repository for tests.
type memRepo struct {
	mu       sync.Mutex
	channels map[string]*Channel
	rules    map[string]*Rule
}

func newMemRepo() *memRepo {
	return &memRepo{channels: map[string]*Channel{}, rules: map[string]*Rule{}}
}

func (m *memRepo) ListChannels(context.Context) ([]*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Channel
	for _, c := range m.channels {
		out = append(out, c.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memRepo) GetChannel(_ context.Context, id string) (*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[id]
	if !ok {
		return nil, ErrChannelNotFound
	}
	return c.Clone(), nil
}

func (m *memRepo) SaveChannel(_ context.Context, c *Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[c.ID] = c.Clone()
	return nil
}

func (m *memRepo) DeleteChannel(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[id]; !ok {
		return ErrChannelNotFound
	}
	delete(m.channels, id)
	for _, r := range m.rules {
		r.ChannelIDs = slices.DeleteFunc(r.ChannelIDs, func(c string) bool { return c == id })
	}
	return nil
}

func (m *memRepo) SaveDeliveryStatus(_ context.Context, id string, st DeliveryStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[id]
	if !ok {
		return ErrChannelNotFound
	}
	c.LastDelivery = &st
	return nil
}

func (m *memRepo) ListRules(context.Context) ([]*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Rule
	for _, r := range m.rules {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memRepo) GetRule(_ context.Context, id string) (*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rules[id]
	if !ok {
		return nil, ErrRuleNotFound
	}
	return r.Clone(), nil
}

func (m *memRepo) SaveRule(_ context.Context, r *Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[r.ID] = r.Clone()
	return nil
}

func (m *memRepo) DeleteRule(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rules[id]; !ok {
		return ErrRuleNotFound
	}
	delete(m.rules, id)
	return nil
}

func webhookChannel(id string) *Channel {
	return &Channel{
		ID: id, Name: "Ops " + id, Type: ChannelWebhook, Enabled: true,
		Webhook: &WebhookConfig{
			URL:     "https://hooks.example.com/x",
			Headers: map[string]string{"Authorization": "Bearer live", "X-Team": "dba"},
			Secret:  "hmac-secret",
		},
	}
}

func TestChannelValidate(t *testing.T) {
	valid := map[string]*Channel{
		"webhook":  webhookChannel("w1"),
		"telegram": {ID: "t1", Name: "tg", Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: testBotToken, ChatID: "@ops_channel"}},
		"email": {ID: "e1", Name: "mail", Type: ChannelEmail, Email: &EmailConfig{
			Host: "smtp.example.com", Port: 587, Security: SecuritySTARTTLS, Username: "u", Password: "p",
			From: "a@example.com", To: []string{"b@example.com"},
		}},
		"twilio": {ID: "s1", Name: "sms", Type: ChannelTwilio, Twilio: &TwilioConfig{
			AccountSID: testTwilioSID, AuthToken: "tok", From: "+15551234567", To: []string{"+905551112233"},
		}},
	}
	for name, ch := range valid {
		if err := ch.Validate(); err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}

	tests := []struct {
		name   string
		mutate func(*Channel)
		want   error
	}{
		{"bad id", func(c *Channel) { c.ID = "a b" }, models.ErrInvalidID},
		{"empty name", func(c *Channel) { c.Name = " " }, ErrInvalidChannelConfig},
		{"bad type", func(c *Channel) { c.Type = "pager" }, ErrInvalidChannelConfig},
		{"missing block", func(c *Channel) { c.Webhook = nil }, ErrInvalidChannelConfig},
		{"two blocks", func(c *Channel) { c.Telegram = &TelegramConfig{} }, ErrInvalidChannelConfig},
		{"ftp scheme", func(c *Channel) { c.Webhook.URL = "ftp://x/y" }, ErrInvalidChannelConfig},
		{"javascript scheme", func(c *Channel) { c.Webhook.URL = "javascript:alert(1)" }, ErrInvalidChannelConfig},
		{"no host", func(c *Channel) { c.Webhook.URL = "https:///path" }, ErrInvalidChannelConfig},
		{"header crlf", func(c *Channel) { c.Webhook.Headers["X-A"] = "v\r\nX-Evil: 1" }, ErrHeaderInjection},
		{"header bad name", func(c *Channel) { c.Webhook.Headers["Bad Name"] = "v" }, ErrInvalidChannelConfig},
		{"header managed", func(c *Channel) { c.Webhook.Headers["X-MongoRescue-Signature"] = "forged" }, ErrInvalidChannelConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := webhookChannel("w1")
			tt.mutate(ch)
			if err := ch.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("Validate = %v; want %v", err, tt.want)
			}
		})
	}

	other := []struct {
		name string
		ch   *Channel
	}{
		{"telegram token", &Channel{ID: "t", Name: "t", Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: "nope", ChatID: "1"}}},
		{"telegram parse mode", &Channel{ID: "t", Name: "t", Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: testBotToken, ChatID: "1", ParseMode: "HTML"}}},
		{"email port", &Channel{ID: "e", Name: "e", Type: ChannelEmail, Email: &EmailConfig{Host: "h", Port: 0, Security: SecurityTLS, From: "a@b.co", To: []string{"c@d.co"}}}},
		{"email security", &Channel{ID: "e", Name: "e", Type: ChannelEmail, Email: &EmailConfig{Host: "h", Port: 25, Security: "ssl", From: "a@b.co", To: []string{"c@d.co"}}}},
		{"email no rcpt", &Channel{ID: "e", Name: "e", Type: ChannelEmail, Email: &EmailConfig{Host: "h", Port: 25, Security: SecurityNone, From: "a@b.co"}}},
		{"twilio sid", &Channel{ID: "s", Name: "s", Type: ChannelTwilio, Twilio: &TwilioConfig{AccountSID: "AC1", AuthToken: "t", From: "+15551234567", To: []string{"+15551234568"}}}},
		{"twilio to", &Channel{ID: "s", Name: "s", Type: ChannelTwilio, Twilio: &TwilioConfig{AccountSID: testTwilioSID, AuthToken: "t", From: "+15551234567", To: []string{"555"}}}},
	}
	for _, tt := range other {
		if err := tt.ch.Validate(); !errors.Is(err, ErrInvalidChannelConfig) {
			t.Errorf("%s: Validate = %v; want ErrInvalidChannelConfig", tt.name, err)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	failed := events.Event{Type: events.BackupFailed, JobID: "job_a"}
	manual := events.Event{Type: events.BackupFailed}
	tests := []struct {
		name string
		rule Rule
		e    events.Event
		want bool
	}{
		{"all jobs", Rule{Enabled: true, Events: []events.EventType{events.BackupFailed}}, failed, true},
		{"all jobs manual", Rule{Enabled: true, Events: []events.EventType{events.BackupFailed}}, manual, true},
		{"disabled", Rule{Enabled: false, Events: []events.EventType{events.BackupFailed}}, failed, false},
		{"other event", Rule{Enabled: true, Events: []events.EventType{events.BackupSucceeded}}, failed, false},
		{"job listed", Rule{Enabled: true, Events: []events.EventType{events.BackupFailed}, JobIDs: []string{"job_b", "job_a"}}, failed, true},
		{"job not listed", Rule{Enabled: true, Events: []events.EventType{events.BackupFailed}, JobIDs: []string{"job_b"}}, failed, false},
		{"job filter excludes manual", Rule{Enabled: true, Events: []events.EventType{events.BackupFailed}, JobIDs: []string{"job_b"}}, manual, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.Matches(tt.e); got != tt.want {
				t.Fatalf("Matches = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestRuleValidate(t *testing.T) {
	ok := Rule{ID: "r1", Name: "n", Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"c"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	bad := []func(*Rule){
		func(r *Rule) { r.Events = nil },
		func(r *Rule) { r.Events = []events.EventType{events.NotificationTest} },
		func(r *Rule) { r.ChannelIDs = nil },
		func(r *Rule) { r.JobIDs = []string{"a\nb"} },
		func(r *Rule) { r.Name = "" },
	}
	for i, mutate := range bad {
		r := *ok.Clone()
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("case %d: Validate = %v; want ErrInvalidRule", i, err)
		}
	}
}

func TestRedactedAndRestoreSecrets(t *testing.T) {
	ch := webhookChannel("w1")
	ch.Webhook.URL = "https://user:pw@hooks.example.com/x?token=abc"
	red := ch.Redacted()
	if red.Webhook.Secret != redact.Mask || red.Webhook.Headers["Authorization"] != redact.Mask {
		t.Errorf("secrets not masked: %+v", red.Webhook)
	}
	if red.Webhook.Headers["X-Team"] != "dba" {
		t.Error("non-secret header must be preserved")
	}
	if strings.Contains(red.Webhook.URL, "pw") {
		t.Errorf("url credentials not masked: %q", red.Webhook.URL)
	}
	if ch.Webhook.Secret != "hmac-secret" {
		t.Error("Redacted must not mutate the receiver")
	}

	// Round-tripping the redacted form restores every stored secret.
	in := red.Clone()
	if err := restoreSecrets(in, ch); err != nil {
		t.Fatalf("restoreSecrets: %v", err)
	}
	if in.Webhook.Secret != "hmac-secret" || in.Webhook.Headers["Authorization"] != "Bearer live" || in.Webhook.URL != ch.Webhook.URL {
		t.Errorf("secrets not restored: %+v", in.Webhook)
	}

	// A masked value without a stored counterpart is rejected.
	if err := restoreSecrets(red.Clone(), nil); !errors.Is(err, ErrMaskedSecret) {
		t.Errorf("restoreSecrets(nil) = %v; want ErrMaskedSecret", err)
	}
	// A type change never carries secrets over.
	tg := &Channel{ID: "w1", Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: redact.Mask, ChatID: "1"}}
	if err := restoreSecrets(tg, ch); !errors.Is(err, ErrMaskedSecret) {
		t.Errorf("type change = %v; want ErrMaskedSecret", err)
	}

	for _, c := range []*Channel{
		{Type: ChannelTelegram, Telegram: &TelegramConfig{BotToken: testBotToken}},
		{Type: ChannelEmail, Email: &EmailConfig{Password: "smtp-pw"}},
		{Type: ChannelTwilio, Twilio: &TwilioConfig{AuthToken: "tw-token"}},
	} {
		raw, err := json.Marshal(c.Redacted())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secretsOf(c) {
			if strings.Contains(string(raw), secret) {
				t.Errorf("secret %q leaked in %s", secret, raw)
			}
		}
		if !strings.Contains(string(raw), redact.Mask) {
			t.Errorf("expected mask in %s", raw)
		}
	}
}

func TestServiceCRUD(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	svc := NewService(repo)

	created, err := svc.CreateChannel(ctx, webhookChannel(""))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if !strings.HasPrefix(created.ID, "ch_") || created.Webhook.Secret != redact.Mask {
		t.Fatalf("unexpected created channel %+v", created.Webhook)
	}
	if _, err = svc.CreateChannel(ctx, webhookChannel(created.ID)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate create = %v; want ErrAlreadyExists", err)
	}
	masked := webhookChannel("")
	masked.Webhook.Secret = redact.Mask
	if _, err = svc.CreateChannel(ctx, masked); !errors.Is(err, ErrMaskedSecret) {
		t.Fatalf("create with mask = %v; want ErrMaskedSecret", err)
	}

	// Update with the masked form keeps the stored secret; a new value replaces it.
	upd := created.Clone()
	upd.Name = "Renamed"
	if _, err = svc.UpdateChannel(ctx, created.ID, upd); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	stored, _ := repo.GetChannel(ctx, created.ID)
	if stored.Name != "Renamed" || stored.Webhook.Secret != "hmac-secret" || stored.Webhook.Headers["Authorization"] != "Bearer live" {
		t.Fatalf("stored secrets lost: %+v", stored.Webhook)
	}
	if _, err = svc.UpdateChannel(ctx, "missing", upd); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("update missing = %v; want ErrChannelNotFound", err)
	}

	rule, err := svc.CreateRule(ctx, &Rule{Name: "r", Enabled: true, Events: []events.EventType{events.BackupFailed, events.BackupFailed}, ChannelIDs: []string{created.ID}})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if len(rule.Events) != 1 {
		t.Errorf("events not de-duplicated: %v", rule.Events)
	}
	if _, err := svc.CreateRule(ctx, &Rule{Name: "r", Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"ghost"}}); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("rule with unknown channel = %v; want ErrInvalidRule", err)
	}

	if err := svc.DeleteChannel(ctx, created.ID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	r, _ := repo.GetRule(ctx, rule.ID)
	if len(r.ChannelIDs) != 0 {
		t.Errorf("deleted channel still referenced: %v", r.ChannelIDs)
	}
	if err := svc.DeleteRule(ctx, rule.ID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if err := svc.DeleteRule(ctx, rule.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("second DeleteRule = %v; want ErrRuleNotFound", err)
	}
}

// outcomeRecorder collects observer calls.
type outcomeRecorder struct {
	mu  sync.Mutex
	got []string
}

func (o *outcomeRecorder) observe(t ChannelType, outcome string) {
	o.mu.Lock()
	o.got = append(o.got, string(t)+":"+outcome)
	o.mu.Unlock()
}

func (o *outcomeRecorder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.got...)
}

func TestServiceDispatch(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fakes := map[string]*fakeNotifier{"a": {}, "b": {failures: 100, err: errors.New("down")}, "off": {}}
	rec := &outcomeRecorder{}
	svc := NewService(repo,
		WithNotifierFactory(func(ch *Channel) (Notifier, error) { return fakes[ch.ID], nil }),
		WithRetryPolicy(RetryPolicy{Retries: 1, BaseBackoff: time.Millisecond, AttemptTimeout: time.Second}),
		WithObserver(rec.observe),
	)
	for _, id := range []string{"a", "b", "off"} {
		ch := webhookChannel(id)
		ch.Enabled = id != "off"
		if _, err := svc.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	// Two overlapping rules: channel "a" must receive a single message.
	for _, r := range []*Rule{
		{ID: "r1", Name: "r1", Enabled: true, Events: []events.EventType{events.BackupFailed}, ChannelIDs: []string{"a", "b", "off"}},
		{ID: "r2", Name: "r2", Enabled: true, Events: []events.EventType{events.BackupFailed}, JobIDs: []string{"job_x"}, ChannelIDs: []string{"a"}},
		{ID: "r3", Name: "r3", Enabled: true, Events: []events.EventType{events.BackupSucceeded}, ChannelIDs: []string{"a"}},
	} {
		if _, err := svc.CreateRule(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()

	svc.HandleEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "job_x", Database: "shop"})
	svc.HandleEvent(ctx, events.Event{Type: events.NotificationTest}) // never rule-dispatched

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fakes["a"].count() != 1 || fakes["off"].count() != 0 {
		t.Errorf("sent a=%d off=%d; want 1, 0", fakes["a"].count(), fakes["off"].count())
	}
	got := rec.snapshot()
	sort.Strings(got)
	if want := []string{"webhook:failure", "webhook:success"}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %v; want %v", got, want)
	}
	a, _ := repo.GetChannel(ctx, "a")
	b, _ := repo.GetChannel(ctx, "b")
	if a.LastDelivery == nil || !a.LastDelivery.Success || b.LastDelivery == nil || b.LastDelivery.Success || b.LastDelivery.Attempts != 2 {
		t.Errorf("unexpected delivery status a=%+v b=%+v", a.LastDelivery, b.LastDelivery)
	}
	if err := svc.Run(ctx); !errors.Is(err, ErrServiceRunning) {
		t.Errorf("second Run = %v; want ErrServiceRunning", err)
	}
}

func TestServiceDropsWhenQueueFullAndDrainsOnShutdown(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fake := &fakeNotifier{}
	rec := &outcomeRecorder{}
	svc := NewService(repo,
		WithNotifierFactory(func(*Channel) (Notifier, error) { return fake, nil }),
		WithQueueSize(2),
		WithWorkers(1),
		WithDrainTimeout(2*time.Second),
		WithObserver(rec.observe),
	)
	if _, err := svc.CreateChannel(ctx, webhookChannel("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRule(ctx, &Rule{ID: "r", Name: "r", Enabled: true, Events: []events.EventType{events.RestoreFailed}, ChannelIDs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}

	// Not running yet: two deliveries fit, the third is dropped without blocking.
	for i := 0; i < 3; i++ {
		svc.HandleEvent(ctx, events.Event{Type: events.RestoreFailed})
	}
	if got := rec.snapshot(); !slices.Equal(got, []string{"webhook:dropped"}) {
		t.Fatalf("outcomes = %v; want one drop", got)
	}

	// Start and immediately stop: queued deliveries are drained before Run returns.
	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := svc.Run(runCtx); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 {
		t.Fatalf("drained deliveries = %d; want 2", fake.count())
	}

	svc.HandleEvent(ctx, events.Event{Type: events.RestoreFailed})
	if got := rec.snapshot(); got[len(got)-1] != "webhook:dropped" {
		t.Errorf("delivery after stop must be dropped, outcomes = %v", got)
	}
}

func TestServiceTestChannel(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fake := &fakeNotifier{failures: 1, err: errors.New("hmac-secret rejected")}
	svc := NewService(repo, WithNotifierFactory(func(*Channel) (Notifier, error) { return fake, nil }))
	ch := webhookChannel("a")
	ch.Enabled = false
	if _, err := svc.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}

	st, err := svc.TestChannel(ctx, "a")
	if err == nil || st == nil || st.Success || st.Attempts != 1 {
		t.Fatalf("first test = %+v, %v; want single failed attempt", st, err)
	}
	if strings.Contains(st.Error, "hmac-secret") || strings.Contains(err.Error(), "hmac-secret") {
		t.Fatalf("channel secret leaked into delivery error: %q", st.Error)
	}
	st, err = svc.TestChannel(ctx, "a")
	if err != nil || !st.Success || st.Event != events.NotificationTest {
		t.Fatalf("second test = %+v, %v", st, err)
	}
	if _, err := svc.TestChannel(ctx, "missing"); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("missing channel = %v", err)
	}
}
