package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// syncPublisher feeds events straight into the metrics collector (no Bus) so tests can
// assert on metric values deterministically.
type syncPublisher struct {
	m     *metrics.Metrics
	count atomic.Int32
}

func (p *syncPublisher) Publish(ctx context.Context, e events.Event) bool {
	p.count.Add(1)
	p.m.ObserveEvent(ctx, e)
	return true
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type notifyFixture struct {
	h       http.Handler
	store   store.Store
	metrics *metrics.Metrics
	pub     *syncPublisher
	logs    *lockedBuffer
}

func newNotifyFixture(t *testing.T, mutate func(*testConfig)) *notifyFixture {
	t.Helper()
	srv, metaStore, _ := setupTestServer(t)
	cfg := newTestConfig()
	if mutate != nil {
		mutate(cfg)
	}
	autoKey := cfg.APIKey == ""
	if autoKey {
		cfg.APIKey = testAPIKey
	}
	f := &notifyFixture{store: metaStore, metrics: metrics.New(metrics.BuildInfo{Version: "test"}), logs: &lockedBuffer{}}
	f.pub = &syncPublisher{m: f.metrics}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	repo, ok := metaStore.(notify.Repository)
	if !ok {
		t.Fatal("the metadata store must implement notify.Repository")
	}
	svc := notify.NewService(repo, notify.WithLogger(logger))
	full := NewServer(bootConfig(), metaStore, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, logger,
		WithSettings(newTestSettings(t, metaStore.(settings.Repository), cfg.Security)),
		WithNotifications(svc),
		WithEventPublisher(f.pub),
		WithMetricsHandler(f.metrics.Handler()),
		WithConnections(srv.connections),
		WithAuth(newTestAuth(t, metaStore.(auth.Repository), cfg.APIKey)),
	)
	f.h = full.Handler()
	if autoKey {
		f.h = keyed{f.h}
	}
	return f
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if v != nil {
		if err := json.Unmarshal(env.Data, v); err != nil {
			t.Fatalf("decode data %s: %v", env.Data, err)
		}
	}
}

func TestNotificationChannelsAPI(t *testing.T) {
	f := newNotifyFixture(t, nil)
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-MongoRescue-Signature") == "" || r.Header.Get("Authorization") != "Bearer hook-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	create := `{"id":"ops_hook","name":"Ops","type":"webhook","enabled":true,
		"webhook":{"url":"` + hook.URL + `","secret":"hmac-key","headers":{"Authorization":"Bearer hook-token"}}}`
	rec := serve(f.h, "POST", "/api/v1/notifications/channels", []byte(create), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created notify.Channel
	decodeData(t, rec, &created)
	if created.Webhook.Secret != redact.Mask || created.Webhook.Headers["Authorization"] != redact.Mask {
		t.Fatalf("secrets not masked in response: %+v", created.Webhook)
	}

	// Duplicate ID conflicts; invalid config and masked secrets on create are 400.
	if rec = serve(f.h, "POST", "/api/v1/notifications/channels", []byte(create), nil); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	bad := []string{
		`{"name":"x","type":"webhook","webhook":{"url":"file:///etc/passwd"}}`,
		`{"name":"x","type":"webhook","webhook":{"url":"https://h","secret":"******"}}`,
		`{"id":"bad id","name":"x","type":"webhook","webhook":{"url":"https://h"}}`,
		`{"name":"x","type":"email","email":{"host":"h","port":25,"security":"none","from":"a@b.co\r\nBcc: c@d.co","to":["e@f.co"]}}`,
		`not json`,
	}
	for _, b := range bad {
		if rec = serve(f.h, "POST", "/api/v1/notifications/channels", []byte(b), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("create %s: got %d; want 400 (%s)", b, rec.Code, rec.Body.String())
		}
	}

	// List never leaks secrets.
	rec = serve(f.h, "GET", "/api/v1/notifications/channels", nil, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hmac-key") || strings.Contains(rec.Body.String(), "hook-token") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Round-trip the masked representation: stored secrets are kept.
	created.Name = "Ops renamed"
	body, _ := json.Marshal(created)
	if rec = serve(f.h, "PUT", "/api/v1/notifications/channels/ops_hook", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	stored, err := f.store.(notify.Repository).GetChannel(context.Background(), "ops_hook")
	if err != nil || stored.Name != "Ops renamed" || stored.Webhook.Secret != "hmac-key" || stored.Webhook.Headers["Authorization"] != "Bearer hook-token" {
		t.Fatalf("keep-secret failed: %+v, %v", stored, err)
	}
	if rec = serve(f.h, "PUT", "/api/v1/notifications/channels/missing", body, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("update missing: %d", rec.Code)
	}

	// Send test: delivered with the real (unmasked) credentials and recorded.
	rec = serve(f.h, "POST", "/api/v1/notifications/channels/ops_hook/test", nil, nil)
	if rec.Code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("test send: %d %s (hits %d)", rec.Code, rec.Body.String(), hits.Load())
	}
	var st notify.DeliveryStatus
	decodeData(t, rec, &st)
	if !st.Success || st.Event != events.NotificationTest {
		t.Fatalf("unexpected status %+v", st)
	}

	// A failing provider yields 502 with the recorded status.
	hook.Close()
	if rec := serve(f.h, "POST", "/api/v1/notifications/channels/ops_hook/test", nil, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("failing test send: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(f.h, "POST", "/api/v1/notifications/channels/missing/test", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("test missing: %d", rec.Code)
	}

	if rec := serve(f.h, "DELETE", "/api/v1/notifications/channels/ops_hook", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := serve(f.h, "DELETE", "/api/v1/notifications/channels/ops_hook", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rec.Code)
	}
}

func TestNotificationRulesAPI(t *testing.T) {
	f := newNotifyFixture(t, nil)
	ch := `{"id":"tg","name":"TG","type":"telegram","enabled":true,"telegram":{"bot_token":"123456:ABCdefGHIjklMNOpqrSTUvwxYZ012345","chat_id":"42"}}`
	if rec := serve(f.h, "POST", "/api/v1/notifications/channels", []byte(ch), nil); rec.Code != http.StatusCreated {
		t.Fatalf("create channel: %d %s", rec.Code, rec.Body.String())
	}

	rule := `{"id":"failures","name":"Failures","enabled":true,"events":["backup.failed","restore.failed"],"channel_ids":["tg"]}`
	rec := serve(f.h, "POST", "/api/v1/notifications/rules", []byte(rule), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rule: %d %s", rec.Code, rec.Body.String())
	}

	bad := []string{
		`{"name":"x","events":["backup.failed"],"channel_ids":["ghost"]}`,
		`{"name":"x","events":["notification.test"],"channel_ids":["tg"]}`,
		`{"name":"x","events":[],"channel_ids":["tg"]}`,
	}
	for _, b := range bad {
		if rec = serve(f.h, "POST", "/api/v1/notifications/rules", []byte(b), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("create rule %s: got %d; want 400", b, rec.Code)
		}
	}

	upd := `{"name":"Failures (shop)","enabled":false,"events":["backup.failed"],"job_ids":["job_shop"],"channel_ids":["tg"]}`
	if rec = serve(f.h, "PUT", "/api/v1/notifications/rules/failures", []byte(upd), nil); rec.Code != http.StatusOK {
		t.Fatalf("update rule: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(f.h, "GET", "/api/v1/notifications/rules", nil, nil)
	var rules []notify.Rule
	decodeData(t, rec, &rules)
	if len(rules) != 1 || rules[0].Enabled || len(rules[0].JobIDs) != 1 {
		t.Fatalf("unexpected rules %+v", rules)
	}

	// Deleting a referenced channel removes it from the rule (documented behaviour).
	if rec = serve(f.h, "DELETE", "/api/v1/notifications/channels/tg", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete channel: %d", rec.Code)
	}
	rec = serve(f.h, "GET", "/api/v1/notifications/rules", nil, nil)
	decodeData(t, rec, &rules)
	if len(rules[0].ChannelIDs) != 0 {
		t.Fatalf("channel still referenced: %v", rules[0].ChannelIDs)
	}

	if rec := serve(f.h, "DELETE", "/api/v1/notifications/rules/failures", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete rule: %d", rec.Code)
	}
	if rec := serve(f.h, "PUT", "/api/v1/notifications/rules/failures", []byte(upd), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("update deleted rule: %d", rec.Code)
	}
}

func TestNotificationsAPIRequiresAuth(t *testing.T) {
	const key = "notify-test-key-0123"
	f := newNotifyFixture(t, func(c *testConfig) { c.APIKey = key })
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/notifications/channels"},
		{"POST", "/api/v1/notifications/channels"},
		{"PUT", "/api/v1/notifications/channels/x"},
		{"DELETE", "/api/v1/notifications/channels/x"},
		{"POST", "/api/v1/notifications/channels/x/test"},
		{"GET", "/api/v1/notifications/rules"},
		{"POST", "/api/v1/notifications/rules"},
		{"PUT", "/api/v1/notifications/rules/x"},
		{"DELETE", "/api/v1/notifications/rules/x"},
	}
	for _, r := range routes {
		if rec := serve(f.h, r.method, r.path, []byte(`{}`), nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without key: %d; want 401", r.method, r.path, rec.Code)
		}
	}
	if rec := serve(f.h, "GET", "/api/v1/notifications/channels", nil, map[string]string{"Authorization": "Bearer " + key}); rec.Code != http.StatusOK {
		t.Errorf("with key: %d", rec.Code)
	}
}

func TestNotificationsUnavailable(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	rec := httptest.NewRecorder()
	srv.buildRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/notifications/rules", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d; want 503 without a notification service", rec.Code)
	}
}

func TestMetricsEndpointAuth(t *testing.T) {
	const key = "metrics-key-0123456789"
	tests := []struct {
		name    string
		apiKey  string
		public  bool
		headers map[string]string
		want    int
	}{
		{"protected without credentials", key, false, nil, http.StatusUnauthorized},
		{"session cookies are not accepted", key, false, map[string]string{"Cookie": SessionCookieName + "=x"}, http.StatusUnauthorized},
		{"protected with bearer", key, false, map[string]string{"Authorization": "Bearer " + key}, http.StatusOK},
		{"public opt-in", key, true, nil, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newNotifyFixture(t, func(c *testConfig) {
				c.APIKey = tt.apiKey
				c.Security.MetricsPublic = tt.public
			})
			rec := serve(f.h, "GET", "/metrics", nil, tt.headers)
			if rec.Code != tt.want {
				t.Fatalf("GET /metrics: %d; want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestManualOperationsEmitEventsAndMetrics(t *testing.T) {
	f := newNotifyFixture(t, nil)

	bkpID := acceptedID(t, serve(f.h, "POST", "/api/v1/backups", []byte(`{"connection_id":"conn_test","database":"users_db","job_id":"attacker-controlled"}`), nil))
	awaitRecord(t, f.h, "/api/v1/backups", bkpID)

	rstID := acceptedID(t, serve(f.h, "POST", "/api/v1/restore", []byte(`{"backup_id":"`+bkpID+`","safe_clone":true}`), nil))
	awaitRecord(t, f.h, "/api/v1/restores", rstID)

	// Events are published right after the final record is persisted.
	for deadline := time.Now().Add(5 * time.Second); f.pub.count.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if f.pub.count.Load() != 2 {
		t.Fatalf("published %d events; want 2", f.pub.count.Load())
	}

	out := serve(f.h, "GET", "/metrics", nil, nil).Body.String()
	for _, want := range []string{
		`mongorescue_backups_total{job="manual",status="succeeded"} 1`,
		`mongorescue_restores_total{status="succeeded"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in /metrics", want)
		}
	}
	if strings.Contains(out, "attacker-controlled") {
		t.Error("unknown client-supplied job IDs must not become metric labels")
	}
}

func TestWebhookURLSecretsNeverExposed(t *testing.T) {
	f := newNotifyFixture(t, nil)
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	secrets := []string{"XXslackSECRETXX", "discordTOKENvalue", "qTOKENq", "hook-bearer"}
	urls := map[string]string{
		"slack":   hook.URL + "/services/T0000/B0000/XXslackSECRETXX",
		"discord": hook.URL + "/api/webhooks/123456/discordTOKENvalue",
		"token":   hook.URL + "/notify?token=qTOKENq&x=1",
	}
	var bodies []string
	check := func(where string) {
		t.Helper()
		all := strings.Join(bodies, "\n") + f.logs.String()
		for _, s := range secrets {
			if strings.Contains(all, s) {
				t.Fatalf("%s: secret %q exposed", where, s)
			}
		}
	}

	for id, u := range urls {
		body := `{"id":"` + id + `","name":"` + id + `","type":"webhook","enabled":true,
			"webhook":{"url":"` + u + `","headers":{"Authorization":"Bearer hook-bearer"}}}`
		rec := serve(f.h, "POST", "/api/v1/notifications/channels", []byte(body), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
		var created notify.Channel
		decodeData(t, rec, &created)

		// Round-trip update with the masked URL and header keeps the real values.
		created.Name = id + " renamed"
		raw, _ := json.Marshal(created)
		rec = serve(f.h, "PUT", "/api/v1/notifications/channels/"+id, raw, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("update %s: %d %s", id, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())

		rec = serve(f.h, "POST", "/api/v1/notifications/channels/"+id+"/test", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("test %s: %d %s", id, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if int(hits.Load()) != len(urls) {
		t.Fatalf("webhook hits = %d; want %d (real URL must be used)", hits.Load(), len(urls))
	}
	check("create/update/test")

	// Transport failure: net/http's *url.Error embeds the full URL.
	hook.Close()
	for id := range urls {
		rec := serve(f.h, "POST", "/api/v1/notifications/channels/"+id+"/test", nil, nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("failing test %s: %d", id, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	rec := serve(f.h, "GET", "/api/v1/notifications/channels", nil, nil)
	bodies = append(bodies, rec.Body.String())
	if !strings.Contains(rec.Body.String(), `"success":false`) {
		t.Fatalf("delivery status should record the failure: %s", rec.Body.String())
	}
	check("failed delivery status and logs")

	// The persisted state keeps the real URL (0600 file), so updates still work.
	stored, err := f.store.(notify.Repository).GetChannel(context.Background(), "slack")
	if err != nil || stored.Webhook.URL != urls["slack"] || strings.Contains(stored.LastDelivery.Error, "XXslackSECRETXX") {
		t.Fatalf("unexpected stored channel %+v, %v", stored, err)
	}
}

func TestMaskedSecretRejectedOnDestinationChange(t *testing.T) {
	f := newNotifyFixture(t, nil)
	create := []string{
		`{"id":"wh","name":"wh","type":"webhook","webhook":{"url":"https://hooks.example.com/x","secret":"hmac-secret","headers":{"Authorization":"Bearer real"}}}`,
		`{"id":"mail","name":"mail","type":"email","email":{"host":"smtp.example.com","port":587,"security":"starttls","username":"ops","password":"smtp-pass","from":"a@example.com","to":["b@example.com"]}}`,
	}
	for _, c := range create {
		if rec := serve(f.h, "POST", "/api/v1/notifications/channels", []byte(c), nil); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
	}

	tests := []struct {
		name, id, body string
		want           int
	}{
		{"webhook header to attacker", "wh", `{"name":"wh","type":"webhook","webhook":{"url":"https://attacker.tld/c","headers":{"Authorization":"******"}}}`, http.StatusBadRequest},
		{"webhook hmac to attacker", "wh", `{"name":"wh","type":"webhook","webhook":{"url":"https://attacker.tld/c","secret":"******"}}`, http.StatusBadRequest},
		{"email password to attacker", "mail", `{"name":"mail","type":"email","email":{"host":"smtp.attacker.tld","port":587,"security":"starttls","username":"ops","password":"******","from":"a@example.com","to":["b@example.com"]}}`, http.StatusBadRequest},
		{"webhook same host other tenant path", "wh", `{"name":"wh","type":"webhook","webhook":{"url":"https://hooks.example.com/tenantB","headers":{"Authorization":"******"}}}`, http.StatusBadRequest},
		{"webhook unchanged destination", "wh", `{"name":"wh2","type":"webhook","webhook":{"url":"https://hooks.example.com/******","secret":"******","headers":{"Authorization":"******"}}}`, http.StatusOK},
		{"email unchanged destination", "mail", `{"name":"mail2","type":"email","email":{"host":"smtp.example.com","port":587,"security":"starttls","username":"ops","password":"******","from":"a@example.com","to":["c@example.com"]}}`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(f.h, "PUT", "/api/v1/notifications/channels/"+tt.id, []byte(tt.body), nil)
			if rec.Code != tt.want {
				t.Fatalf("PUT: %d; want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}

	repo := f.store.(notify.Repository)
	wh, _ := repo.GetChannel(context.Background(), "wh")
	mail, _ := repo.GetChannel(context.Background(), "mail")
	if wh.Webhook.URL != "https://hooks.example.com/x" || wh.Webhook.Secret != "hmac-secret" || wh.Webhook.Headers["Authorization"] != "Bearer real" {
		t.Errorf("webhook secrets not kept: %+v", wh.Webhook)
	}
	if mail.Email.Password != "smtp-pass" || mail.Email.To[0] != "c@example.com" {
		t.Errorf("email secret not kept: %+v", mail.Email)
	}
}
