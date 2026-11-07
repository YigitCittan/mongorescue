package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestOversizedBodiesAreRefused sends a body above the limit to every endpoint that
// decodes a job, backup or restore request and expects 413; malformed JSON stays 400.
func TestOversizedBodiesAreRefused(t *testing.T) {
	h, _ := newChainServer(t, nil)
	big := []byte(`{"name":"` + strings.Repeat("a", maxAPIBody) + `"}`)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/jobs"},
		{"PUT", "/api/v1/jobs/job-1"},
		{"POST", "/api/v1/backups"},
		{"POST", "/api/v1/restore"},
		{"POST", "/api/v1/restores/preflight"},
	} {
		t.Run("oversized "+tc.method+" "+tc.path, func(t *testing.T) {
			rec := serve(h, tc.method, tc.path, big, nil)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d (%s); want 413", rec.Code, rec.Body.String())
			}
		})
		t.Run("malformed "+tc.method+" "+tc.path, func(t *testing.T) {
			rec := serve(h, tc.method, tc.path, []byte(`{`), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s); want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestReadJSONLimit pins the helper the pin, reject and import endpoints share: an
// oversized body is answered with 413 instead of being truncated.
func TestReadJSONLimit(t *testing.T) {
	var v struct{ Note string }
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"note":"`+strings.Repeat("x", 100)+`"}`))
	err := readJSON(rec, r, &v, 32)
	if err == nil {
		t.Fatal("oversized body decoded")
	}
	writeDecodeError(rec, err, "invalid request json")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rec.Code)
	}
	empty := httptest.NewRequest("POST", "/", strings.NewReader(""))
	if err := readJSON(httptest.NewRecorder(), empty, &v, 32); !errors.Is(err, io.EOF) {
		t.Fatalf("empty body error = %v; want io.EOF", err)
	}
}

// TestMCPBodyIsBounded checks that the handler behind /mcp cannot read a body above
// maxMCPBody: the SDK's Streamable HTTP handler applies no limit of its own.
func TestMCPBodyIsBounded(t *testing.T) {
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	a := newTestAuth(t, st, "")
	var readErr error
	stub := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
	})
	srv := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, nil,
		WithAuth(a), WithSettings(newTestSettings(t, st, newTestConfig().Security)), WithMCPHandler(stub), WithAudit(audit.NewService(st, nil)))
	_, key, err := a.CreateAPIKey(context.Background(), auth.SystemPrincipal(), "mcp", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", MCPPath, strings.NewReader(strings.Repeat("x", maxMCPBody+1)))
	req.Header.Set("X-API-Key", key)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("read error = %v; want *http.MaxBytesError", readErr)
	}
}

func TestNewHTTPServerIdleTimeout(t *testing.T) {
	srv := NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.IdleTimeout <= time.Minute {
		t.Fatalf("IdleTimeout = %s; want above the 60 s idle timeout of common proxies", srv.IdleTimeout)
	}
}

// TestHSTS checks Strict-Transport-Security: only over TLS (directly or through a
// trusted proxy reporting https), never on plain HTTP even with secure_cookies=always,
// and not at all with security.hsts_max_age 0, the default.
func TestHSTS(t *testing.T) {
	const day = "max-age=31536000"
	on := func(c *testConfig) { c.Security.HSTSMaxAge = settings.RecommendedHSTSMaxAge }
	proxy := func(c *testConfig) { on(c); c.Security.TrustedProxies = []string{"192.0.2.0/24"} }
	for _, tc := range []struct {
		name    string
		mutate  func(*testConfig)
		tls     bool
		headers map[string]string
		want    string
	}{
		{"default off", nil, true, nil, ""},
		{"plain http", on, false, nil, ""},
		{"direct tls", on, true, nil, day},
		{"trusted proxy https", proxy, false, map[string]string{"X-Forwarded-Proto": "https"}, day},
		{"trusted proxy http", proxy, false, map[string]string{"X-Forwarded-Proto": "http"}, ""},
		{"untrusted https header", on, false, map[string]string{"X-Forwarded-Proto": "https"}, ""},
		{"secure cookies always on http", func(c *testConfig) { on(c); c.Security.SecureCookies = settings.CookiesAlways }, false, nil, ""},
		{"disabled", func(c *testConfig) { c.Security.HSTSMaxAge = 0 }, true, nil, ""},
		{"custom age", func(c *testConfig) { c.Security.HSTSMaxAge = 600 }, true, nil, "max-age=600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newChainServer(t, tc.mutate)
			req := httptest.NewRequest("GET", "/api/v1/health", nil)
			req.RemoteAddr = "192.0.2.10:4000"
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Fatalf("Strict-Transport-Security = %q; want %q", got, tc.want)
			}
		})
	}
}
