package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestExpiredAPIKeysAreRefusedWithACode proves the API key expiry over HTTP: a key
// created with expires_in_days shows its expiry in the list, in /auth/me and in
// the settings warnings; once expired it is refused with 401 and the code
// api_key_expired on every route, and the refusal is audited with the key.
func TestExpiredAPIKeysAreRefusedWithACode(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	clock := &testClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	set := newTestSettings(t, st, newTestConfig().Security)
	svc := newTestAuth(t, st, "", auth.WithClock(clock.Now), auth.WithAPIKeyPolicy(func() auth.APIKeyPolicy {
		sec := set.Current().Security
		return auth.APIKeyPolicy{MaxLifetime: sec.APIKeyMaxLifetime(), WarnBefore: sec.APIKeyExpiryWarning()}
	}))
	log := auditlog.New(auditlog.Config{Repo: st, Logger: slog.New(slog.DiscardHandler)})
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(set),
		WithAudit(audit.NewService(st, slog.New(slog.DiscardHandler), audit.WithObserver(log.Mirror()))), WithAuditLog(log))
	f := &auditLogFixture{authFixture: &authFixture{h: full.Handler(), auth: svc}, log: log, srv: full}
	b := f.browser(t)
	b.setup(f.authFixture)

	// Both forms of the expiry at once are refused, and so is an expiry in the past.
	if rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "x", "expires_in_days": 1, "expires_at": clock.Now().Add(time.Hour)}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("both expiries = %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "x", "expires_at": clock.Now().Add(-time.Hour)}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("a past expiry = %d %s", rec.Code, rec.Body.String())
	}
	rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "nightly", "scope": "read", "expires_in_days": 2}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created createdAPIKey
	decodeData(t, rec, &created)
	want := clock.Now().Add(48 * time.Hour)
	if created.APIKey.ExpiresAt == nil || !created.APIKey.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v; want %v", created.APIKey.ExpiresAt, want)
	}
	bearer := map[string]string{"Authorization": "Bearer " + created.Key}
	key := &browser{t: t, h: f.h, ip: "192.0.2.7"}
	var me meResponse
	decodeData(t, key.do("GET", "/api/v1/auth/me", nil, bearer), &me)
	if me.KeyExpiresAt == nil || !me.KeyExpiresAt.Equal(want) {
		t.Errorf("me key_expires_at = %v", me.KeyExpiresAt)
	}

	// Within the warning window (14 days by default) the settings warn about it.
	var resp settingsResponse
	decodeData(t, b.do("GET", "/api/v1/settings", nil, nil), &resp)
	found := false
	for _, w := range resp.Warnings {
		if w.ID == settings.WarningAPIKeysExpiring && len(w.APIKeys) == 1 && w.APIKeys[0].ID == created.APIKey.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %+v; want %s naming the key", resp.Warnings, settings.WarningAPIKeysExpiring)
	}

	clock.Advance(48 * time.Hour)
	for _, path := range []string{"/api/v1/jobs", "/api/v1/auth/me"} {
		rec = key.do("GET", path, nil, bearer)
		var env apiResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != http.StatusUnauthorized || env.Code != codeAPIKeyExpired {
			t.Fatalf("GET %s with an expired key = %d %s; want 401 %s", path, rec.Code, rec.Body.String(), codeAPIKeyExpired)
		}
	}
	rec = key.do("POST", "/api/v1/backups", map[string]string{}, bearer)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST with an expired key = %d", rec.Code)
	}
	refused := 0
	for _, e := range f.events(t) {
		if e.Targets[targetReason] != reasonAPIKeyExpired {
			continue
		}
		refused++
		if e.ActorKind != auditlog.ActorAPIKey || e.ActorKeyID != created.APIKey.ID || e.ActorKeyName != "nightly" ||
			e.Status != http.StatusUnauthorized || e.Outcome != auditlog.OutcomeDenied {
			t.Errorf("refusal entry = %+v", e)
		}
	}
	if refused == 0 {
		t.Fatal("the refusals of the expired key were not audited")
	}
	// Expired-key refusals are charged to the budget of the client address, like
	// any other request without valid credentials.
	var last *httptest.ResponseRecorder
	for range anonymousRateLimit.Burst {
		last = key.do("GET", "/api/v1/jobs", nil, bearer)
	}
	assertRateLimited(t, last, "an address sending an expired key over the anonymous burst")
}

// TestAMaximumLifetimeRefusesKeysWithoutAnExpiry proves the cap over HTTP.
func TestAMaximumLifetimeRefusesKeysWithoutAnExpiry(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	set := newTestSettings(t, st, newTestConfig().Security)
	days := 30
	if _, err := set.Update(t.Context(), settings.Patch{Security: &settings.SecurityPatch{APIKeyMaxLifetimeDays: &days}}); err != nil {
		t.Fatal(err)
	}
	svc := newTestAuth(t, st, "", auth.WithAPIKeyPolicy(func() auth.APIKeyPolicy {
		return auth.APIKeyPolicy{MaxLifetime: set.Current().Security.APIKeyMaxLifetime()}
	}))
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(set))
	f := &authFixture{h: full.Handler(), auth: svc}
	b := f.browser(t)
	b.setup(f)
	if rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "forever"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no expiry under a maximum = %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "long", "expires_in_days": 31}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("beyond the maximum = %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.do("POST", "/api/v1/api-keys", map[string]any{"name": "ok", "expires_in_days": 30}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("within the maximum = %d %s", rec.Code, rec.Body.String())
	}
}
