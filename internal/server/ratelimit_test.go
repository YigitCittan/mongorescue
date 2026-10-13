package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// rateFixture is a full server with small rate limits and a recording observer.
type rateFixture struct {
	*authFixture
	log *auditlog.Service

	mu       sync.Mutex
	observed []string
}

func newRateFixture(t *testing.T, general, burst, work, workBurst int) *rateFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	cfg := newTestConfig()
	cfg.Security.APIRateLimitPerMinute, cfg.Security.APIRateLimitBurst = general, burst
	cfg.Security.APIWorkRateLimitPerMinute, cfg.Security.APIWorkRateLimitBurst = work, workBurst
	svc := newTestAuth(t, st, testAPIKey)
	f := &rateFixture{log: auditlog.New(auditlog.Config{Repo: st, Logger: slog.New(slog.DiscardHandler)})}
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, cfg.Security)), WithAuditLog(f.log),
		WithRateLimitObserver(func(caller, budget string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.observed = append(f.observed, caller+"/"+budget)
		}))
	f.authFixture = &authFixture{h: full.Handler(), auth: svc}
	return f
}

// request sends method path with the API key (none when key is "") from ip.
func (f *rateFixture) request(method, path, key, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.RemoteAddr = ip + ":50000"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// assertRateLimited fails unless rec is a 429 with Retry-After and the code.
func assertRateLimited(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	var env apiResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusTooManyRequests || env.Code != codeRateLimited || err != nil || secs < 1 {
		t.Fatalf("%s: %d %q Retry-After %q; want 429 %s with Retry-After", what, rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), codeRateLimited)
	}
}

func TestRateLimitPerAPIKey(t *testing.T) {
	f := newRateFixture(t, 1, 3, 0, 1)
	for i := range 3 {
		if rec := f.request("GET", "/api/v1/jobs", testAPIKey, "192.0.2.1"); rec.Code != http.StatusOK {
			t.Fatalf("request %d within the burst = %d", i, rec.Code)
		}
	}
	assertRateLimited(t, f.request("GET", "/api/v1/jobs", testAPIKey, "192.0.2.1"), "over the burst")
	// Another address does not help: the budget is the key's.
	assertRateLimited(t, f.request("GET", "/api/v1/stats", testAPIKey, "198.51.100.9"), "the same key from another address")
	// The health check and /metrics are never limited.
	for range 5 {
		if rec := f.request("GET", healthPath, "", "192.0.2.1"); rec.Code != http.StatusOK {
			t.Fatalf("health = %d", rec.Code)
		}
	}
	// A second key has a budget of its own.
	admin := f.browser(t)
	admin.setup(f.authFixture)
	rec := admin.do("POST", "/api/v1/api-keys", map[string]string{"name": "other", "scope": "read"}, nil)
	var created createdAPIKey
	decodeData(t, rec, &created)
	if rec := f.request("GET", "/api/v1/jobs", created.Key, "192.0.2.1"); rec.Code != http.StatusOK {
		t.Fatalf("a second key = %d %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.observed) != 2 || f.observed[0] != "api_key/general" {
		t.Errorf("observed %v; want two api_key/general refusals", f.observed)
	}
}

func TestRateLimitWorkRoutesHaveATighterBudget(t *testing.T) {
	f := newRateFixture(t, 1000, 1000, 1, 2)
	for i := range 2 {
		if rec := f.request("POST", "/api/v1/backups", testAPIKey, "192.0.2.1"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("work request %d within the work burst was limited", i)
		}
	}
	rec := f.request("POST", "/api/v1/backups", testAPIKey, "192.0.2.1")
	assertRateLimited(t, rec, "over the work burst")
	if !strings.Contains(rec.Body.String(), "work rate limit") {
		t.Errorf("refusal %s does not name the work budget", rec.Body.String())
	}
	if rec := f.request("GET", "/api/v1/jobs", testAPIKey, "192.0.2.1"); rec.Code != http.StatusOK {
		t.Fatalf("a read after the work budget ran out = %d", rec.Code)
	}
}

func TestRateLimitPerSessionAndAudit(t *testing.T) {
	f := newRateFixture(t, 1, 4, 0, 1)
	b := f.browser(t)
	b.setup(f.authFixture) // the setup itself spends the anonymous budget, not the session's
	var last *httptest.ResponseRecorder
	for range 6 {
		last = b.do("GET", "/api/v1/jobs", nil, nil)
	}
	assertRateLimited(t, last, "a session over its burst")
	// Reads are not audited, but the first refusal per minute is.
	var refusals []*auditlog.Event
	if err := f.log.Export(t.Context(), auditlog.Filter{}, func(e *auditlog.Event) error {
		if e.Outcome == auditlog.OutcomeRateLimited {
			refusals = append(refusals, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 || refusals[0].ActorKind != auditlog.ActorUser || refusals[0].Status != http.StatusTooManyRequests ||
		refusals[0].Targets[targetReason] != reasonRateLimited || refusals[0].Action != "GET /api/v1/jobs" {
		t.Fatalf("audited refusals = %+v; want one of the session", refusals)
	}
}

func TestRateLimitPerAddressWithoutCredentials(t *testing.T) {
	f := newRateFixture(t, 1000, 1000, 0, 1)
	var last *httptest.ResponseRecorder
	for range anonymousRateLimit.Burst + 1 {
		last = f.request("GET", "/api/v1/setup/status", "", "203.0.113.5")
	}
	assertRateLimited(t, last, "an address over the anonymous burst")
	assertRateLimited(t, f.request("GET", "/api/v1/jobs", "wrong-key-of-some-length", "203.0.113.5"), "a wrong key from that address")
	if rec := f.request("GET", "/api/v1/setup/status", "", "203.0.113.6"); rec.Code != http.StatusOK {
		t.Fatalf("another address = %d", rec.Code)
	}
	if rec := f.request("GET", healthPath, "", "203.0.113.5"); rec.Code != http.StatusOK {
		t.Fatalf("health from the limited address = %d", rec.Code)
	}
	// Valid credentials are not held back by their address's anonymous budget.
	if rec := f.request("GET", "/api/v1/jobs", testAPIKey, "203.0.113.5"); rec.Code != http.StatusOK {
		t.Fatalf("a valid key from the limited address = %d", rec.Code)
	}
}

func TestRateLimitOff(t *testing.T) {
	f := newRateFixture(t, 0, 1, 1, 1)
	for i := range 50 {
		if rec := f.request("POST", "/api/v1/backups", testAPIKey, "192.0.2.1"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was limited with security.api_rate_limit_per_minute 0", i)
		}
	}
}

func TestAddressKeyGroupsIPv6By64(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":                "192.0.2.7",
		"::ffff:192.0.2.7":         "192.0.2.7",
		"2001:db8:1:2:3:4:5:6":     "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":     "2001:db8:1:2::/64",
		"not-an-address":           "not-an-address",
		"2001:db8:1:3:3:4:5:6%eth": "2001:db8:1:3::/64",
	} {
		if got := addressKey(in); got != want {
			t.Errorf("addressKey(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestWorkRoutesAreRegisteredAndComplete keeps workRoutes in step with the routes:
// every entry is a registered route, and every operator route that starts work is
// in it (cancelling and pinning are cheap and use the general budget).
func TestWorkRoutesAreRegisteredAndComplete(t *testing.T) {
	f := newScopeFixture(t)
	patterns := f.authenticatedPatterns()
	for p := range workRoutes {
		if !slices.Contains(patterns, p) {
			t.Errorf("workRoutes has a stale entry %q", p)
		}
		if strings.HasPrefix(p, "GET ") {
			t.Errorf("workRoutes holds the read %q; reads use the general budget", p)
		}
	}
	for _, p := range operatorRoutes {
		cheap := strings.HasSuffix(p, "/cancel") || strings.HasSuffix(p, "/pin")
		if !cheap && !workRoutes[p] {
			t.Errorf("operator route %q starts work but is not in workRoutes", p)
		}
	}
	for p, need := range routeScopes {
		if need == auth.ScopeOperator && strings.HasPrefix(p, "POST ") && !slices.Contains(operatorRoutes, p) {
			t.Errorf("operator route %q is missing from operatorRoutes", p)
		}
	}
}
