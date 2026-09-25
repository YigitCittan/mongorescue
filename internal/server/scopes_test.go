package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// operatorRoutes are the only routes an operator key may call beyond reads.
var operatorRoutes = []string{
	"POST /api/v1/backups",
	"POST /api/v1/jobs/{id}/run",
	"POST /api/v1/restore",
}

// adminOnlyReads are GET routes that need more than the read scope.
var adminOnlyReads = []string{}

// scopeFixture serves the full middleware chain with one API key per scope.
type scopeFixture struct {
	srv  *Server
	h    http.Handler
	keys map[auth.Scope]string
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	svc := newTestAuth(t, st, "")
	metricsOK := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithMetricsHandler(metricsOK))
	f := &scopeFixture{srv: srv, h: srv.Handler(), keys: map[auth.Scope]string{}}
	for _, scope := range auth.Scopes() {
		_, plain, err := svc.CreateAPIKey(context.Background(), auth.SystemPrincipal(), string(scope)+" key", scope)
		if err != nil {
			t.Fatal(err)
		}
		f.keys[scope] = plain
	}
	return f
}

// concrete turns a route pattern into a method and a request path.
func concrete(pattern string) (method, path string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = http.MethodPost, pattern
	}
	r := strings.NewReplacer("{id}", "does_not_exist", "{db}", "shop")
	return method, r.Replace(path)
}

// scopeRefused reports whether rec is the middleware's scope refusal.
func scopeRefused(code int, body string) bool {
	return code == http.StatusForbidden && strings.Contains(body, "scope")
}

// authenticatedPatterns returns every registered pattern that needs credentials.
func (f *scopeFixture) authenticatedPatterns() []string {
	var out []string
	for _, p := range f.srv.patterns {
		_, path := concrete(p)
		if publicPaths[path] || p == "GET /" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func TestEveryRouteHasAScope(t *testing.T) {
	f := newScopeFixture(t)
	patterns := f.authenticatedPatterns()
	if len(patterns) < 45 {
		t.Fatalf("only %d authenticated routes registered; the fixture is missing some", len(patterns))
	}
	for _, p := range patterns {
		need, ok := routeScopes[p]
		if !ok {
			t.Errorf("route %q has no entry in routeScopes", p)
			continue
		}
		method, _ := concrete(p)
		switch {
		case slices.Contains(operatorRoutes, p):
			if need != auth.ScopeOperator {
				t.Errorf("%q needs %q; operator routes need operator", p, need)
			}
		case method == http.MethodGet && !slices.Contains(adminOnlyReads, p):
			if need != auth.ScopeRead {
				t.Errorf("%q needs %q; reads need read", p, need)
			}
		default:
			if need != auth.ScopeAdmin {
				t.Errorf("%q needs %q; every other change needs admin", p, need)
			}
		}
	}
	for p := range routeScopes {
		if !slices.Contains(patterns, p) {
			t.Errorf("routeScopes has a stale entry %q", p)
		}
	}
}

func TestScopesAreEnforcedForEveryRoute(t *testing.T) {
	f := newScopeFixture(t)
	for _, p := range f.authenticatedPatterns() {
		need := requiredScope(p)
		method, path := concrete(p)
		for _, have := range auth.Scopes() {
			rec := serve(f.h, method, path, []byte(`{}`), map[string]string{
				"Authorization": "Bearer " + f.keys[have],
				"Content-Type":  "application/json",
			})
			refused := scopeRefused(rec.Code, rec.Body.String())
			if want := !have.Allows(need); refused != want {
				t.Errorf("%s %s with a %s key: status %d %s; refused=%v, want %v", method, path, have, rec.Code, rec.Body.String(), refused, want)
			}
		}
	}
}

func TestOperatorCannotDeleteOrReconfigure(t *testing.T) {
	f := newScopeFixture(t)
	op := map[string]string{"X-API-Key": f.keys[auth.ScopeOperator], "Content-Type": "application/json"}
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/api/v1/backups/bkp_1"},
		{"DELETE", "/api/v1/jobs/job_1"},
		{"DELETE", "/api/v1/connections/" + testConnID},
		{"PUT", "/api/v1/settings"},
		{"POST", "/api/v1/api-keys"},
	} {
		if rec := serve(f.h, tc.method, tc.path, []byte(`{}`), op); rec.Code != http.StatusForbidden {
			t.Errorf("operator %s %s = %d %s; want 403", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	read := map[string]string{"X-API-Key": f.keys[auth.ScopeRead], "Content-Type": "application/json"}
	rec := serve(f.h, "POST", "/api/v1/backups", []byte(`{"connection_id":"`+testConnID+`","database":"shop"}`), read)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `\"read\"`) {
		t.Fatalf("read key starting a backup = %d %s; want 403 naming the scope", rec.Code, rec.Body.String())
	}
}

func TestInPlaceRestoreNeedsAdmin(t *testing.T) {
	f := newScopeFixture(t)
	body := []byte(`{"backup_id":"bkp_missing","safe_clone":false,"confirm_in_place":true}`)
	op := map[string]string{"X-API-Key": f.keys[auth.ScopeOperator], "Content-Type": "application/json"}
	rec := serve(f.h, "POST", "/api/v1/restore", body, op)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "in-place") {
		t.Fatalf("operator in-place restore = %d %s; want 403", rec.Code, rec.Body.String())
	}
	admin := map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin], "Content-Type": "application/json"}
	if rec := serve(f.h, "POST", "/api/v1/restore", body, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("admin in-place restore of a missing backup = %d %s; want 404", rec.Code, rec.Body.String())
	}
}

func TestCreateAPIKeyScope(t *testing.T) {
	f := newAuthFixture(t, nil)
	b := f.browser(t)
	b.setup(f)
	for _, tc := range []struct {
		body  map[string]string
		code  int
		scope string
	}{
		{map[string]string{"name": "default"}, http.StatusCreated, "read"},
		{map[string]string{"name": "ops", "scope": "operator"}, http.StatusCreated, "operator"},
		{map[string]string{"name": "root", "scope": "admin"}, http.StatusCreated, "admin"},
		{map[string]string{"name": "bad", "scope": "superuser"}, http.StatusBadRequest, ""},
	} {
		rec := b.do("POST", "/api/v1/api-keys", tc.body, nil)
		if rec.Code != tc.code {
			t.Fatalf("%s: status %d %s", tc.body, rec.Code, rec.Body.String())
		}
		if tc.scope != "" && !strings.Contains(rec.Body.String(), `"scope":"`+tc.scope+`"`) {
			t.Fatalf("%s: response lacks scope %q: %s", tc.body, tc.scope, rec.Body.String())
		}
	}
}
