package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// mcpFixture serves the full chain with a stub MCP handler that reports the scope of
// the principal it received.
type mcpFixture struct {
	h        http.Handler
	settings *settings.Service
	audit    *audit.Service
	auth     *auth.Service
	keys     map[auth.Scope]string
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	f := &mcpFixture{auth: newTestAuth(t, st, ""), settings: newTestSettings(t, st, newTestConfig().Security),
		audit: audit.NewService(st, nil), keys: map[auth.Scope]string{}}
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, _ = w.Write([]byte("scope=" + string(p.Scope)))
	})
	srv := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, nil,
		WithAuth(f.auth), WithSettings(f.settings), WithMCPHandler(stub), WithAudit(f.audit))
	f.h = srv.Handler()
	for _, scope := range auth.Scopes() {
		_, plain, err := f.auth.CreateAPIKey(context.Background(), auth.SystemPrincipal(), string(scope), scope)
		if err != nil {
			t.Fatal(err)
		}
		f.keys[scope] = plain
	}
	return f
}

func (f *mcpFixture) post(t *testing.T, headers map[string]string, local string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", MCPPath, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if local != "" {
		addr, err := net.ResolveTCPAddr("tcp", local)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestMCPEndpointAcceptsAPIKeysOnly(t *testing.T) {
	f := newMCPFixture(t)
	rec := f.post(t, nil, "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no credentials = %d; want 401 with WWW-Authenticate", rec.Code)
	}

	// A logged-in browser session is not accepted, even with its CSRF token.
	b := (&authFixture{h: f.h, auth: f.auth}).browser(t)
	b.setup(&authFixture{h: f.h, auth: f.auth})
	req := httptest.NewRequest("POST", MCPPath, strings.NewReader(`{}`))
	req.AddCookie(b.cookie)
	req.Header.Set(CSRFHeader, b.csrf)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie on /mcp = %d; want 401", rec.Code)
	}

	for _, scope := range auth.Scopes() {
		rec := f.post(t, map[string]string{"Authorization": "Bearer " + f.keys[scope]}, "")
		if rec.Code != http.StatusOK || rec.Body.String() != "scope="+string(scope) {
			t.Fatalf("%s key = %d %s; want the handler to see the principal", scope, rec.Code, rec.Body.String())
		}
	}
}

func TestMCPEndpointPolicy(t *testing.T) {
	f := newMCPFixture(t)
	key := map[string]string{"X-API-Key": f.keys[auth.ScopeRead]}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"X-API-Key": f.keys[auth.ScopeRead]}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	if rec := f.post(t, with(map[string]string{"Origin": "https://evil.example"}), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin browser request = %d; want 403", rec.Code)
	}
	if rec := f.post(t, with(map[string]string{"Origin": "http://example.com"}), ""); rec.Code != http.StatusOK {
		t.Fatalf("same-origin request = %d %s; want 200", rec.Code, rec.Body.String())
	}

	// DNS rebinding: arrived on loopback, but names a foreign host.
	rec := f.post(t, key, "127.0.0.1:8080")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Host") {
		t.Fatalf("loopback listener with Host example.com = %d; want 403", rec.Code)
	}
	localHost := with(map[string]string{"Host": "localhost:8080"})
	req := httptest.NewRequest("POST", "http://localhost:8080"+MCPPath, strings.NewReader(`{}`))
	for k, v := range localHost {
		req.Header.Set(k, v)
	}
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:8080")
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback listener with Host localhost = %d; want 200", rec.Code)
	}
	trust := true
	if _, err := f.settings.Update(context.Background(), settings.Patch{Security: &settings.SecurityPatch{TrustProxyHeaders: &trust}}); err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, key, "127.0.0.1:8080"); rec.Code != http.StatusOK {
		t.Fatalf("behind a trusted reverse proxy = %d; want 200", rec.Code)
	}

	off := false
	if _, err := f.settings.Update(context.Background(), settings.Patch{Security: &settings.SecurityPatch{MCPEnabled: &off}}); err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, key, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("disabled endpoint = %d %s; want 403", rec.Code, rec.Body.String())
	}
}

func TestAuditEndpoint(t *testing.T) {
	f := newMCPFixture(t)
	f.audit.Record(context.Background(), audit.Entry{APIKeyID: "key_1", APIKeyName: "ci", Transport: audit.TransportStdio,
		Tool: "start_backup", Arguments: json.RawMessage(`{"database":"shop"}`), Result: audit.ResultOK})
	admin := map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin]}
	rec := serve(f.h, "GET", "/api/v1/audit?limit=10", nil, admin)
	var res struct {
		Data []audit.Entry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK || len(res.Data) != 1 || res.Data[0].Tool != "start_backup" {
		t.Fatalf("GET /api/v1/audit = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(f.h, "GET", "/api/v1/audit?limit=0", nil, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d; want 400", rec.Code)
	}
	if rec := serve(f.h, "GET", "/api/v1/audit", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeOperator]}); rec.Code != http.StatusForbidden {
		t.Fatalf("operator reading the audit log = %d; want 403", rec.Code)
	}
}
