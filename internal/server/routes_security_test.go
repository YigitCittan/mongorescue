package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// unsafeMethod reports whether method changes state (and so needs a CSRF token when
// it is authenticated by a session cookie).
func unsafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// TestRouteSecurityMatrix walks every registered route and checks the whole access
// matrix against routeScopes: no credentials, every scope below the required one, a
// session cookie without or with a wrong CSRF token, and the required scope. A route
// registered without an entry in routeScopes fails here, so the table cannot fall
// behind the router.
func TestRouteSecurityMatrix(t *testing.T) {
	f := newScopeFixture(t)
	res, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := SessionCookieName + "=" + res.Token
	patterns := f.authenticatedPatterns()
	if len(patterns) < len(routeScopes) {
		t.Fatalf("%d authenticated routes registered; routeScopes lists %d", len(patterns), len(routeScopes))
	}
	jsonBody := map[string]string{"Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	for _, p := range patterns {
		need, ok := routeScopes[p]
		if !ok {
			t.Errorf("%s: registered without an entry in routeScopes; add one with the scope it needs", p)
			continue
		}
		method, path := concrete(p)
		t.Run(p, func(t *testing.T) {
			// 1. No credentials.
			rec := serve(f.h, method, path, []byte(`{}`), jsonBody)
			switch {
			case p == "GET "+meRoute:
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"user":null`) {
					t.Errorf("anonymous: %d %s; want 200 with an empty session", rec.Code, rec.Body)
				}
			case rec.Code != http.StatusUnauthorized:
				t.Errorf("anonymous: %d %s; want 401", rec.Code, rec.Body)
			}
			// A malformed or unknown key is no better than none.
			for _, bad := range []string{"mr_notakey", "Bearer", f.keys[auth.ScopeAdmin] + "x"} {
				rec = serve(f.h, method, path, []byte(`{}`), with(map[string]string{"Authorization": "Bearer " + bad}))
				if p == "GET "+meRoute {
					if strings.Contains(rec.Body.String(), `"auth":"api_key"`) {
						t.Errorf("bad key %q is reported as signed in: %s", bad, rec.Body)
					}
				} else if rec.Code != http.StatusUnauthorized {
					t.Errorf("bad key %q: %d %s; want 401", bad, rec.Code, rec.Body)
				}
			}

			// 2. Every scope below the required one.
			for _, have := range auth.Scopes() {
				if have.Allows(need) {
					continue
				}
				rec = serve(f.h, method, path, []byte(`{}`), with(map[string]string{"Authorization": "Bearer " + f.keys[have]}))
				if !scopeRefused(rec.Code, rec.Body.String()) {
					t.Errorf("%s key (needs %s): %d %s; want 403", have, need, rec.Code, rec.Body)
				}
			}

			// 3. A session cookie on an unsafe method needs the CSRF token.
			if unsafeMethod(method) {
				for name, token := range map[string]string{"no": "", "a wrong": "0" + res.CSRFToken[1:], "a truncated": res.CSRFToken[:8]} {
					h := with(map[string]string{"Cookie": cookie})
					if token != "" {
						h[CSRFHeader] = token
					}
					rec = serve(f.h, method, path, []byte(`{}`), h)
					want := http.StatusForbidden
					if path == MCPPath || path == "/metrics" {
						want = http.StatusUnauthorized // bearer tokens only
					}
					if rec.Code != want {
						t.Errorf("session with %s CSRF token: %d %s; want %d", name, rec.Code, rec.Body, want)
					}
				}
			}

			// 4. The required scope gets through the middleware.
			rec = serve(f.h, method, path, []byte(`{}`), with(map[string]string{"Authorization": "Bearer " + f.keys[need]}))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s key: %d %s; want the handler's answer", need, rec.Code, rec.Body)
			}
		})
	}
}

// TestSessionCannotReachMachineEndpoints checks that /metrics and /mcp never accept a
// session cookie, so a browser session cannot be ridden cross-site into them.
func TestSessionCannotReachMachineEndpoints(t *testing.T) {
	f := newScopeFixture(t)
	res, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ method, path string }{{"GET", "/metrics"}, {"POST", MCPPath}, {"GET", MCPPath}} {
		rec := serve(f.h, target.method, target.path, []byte(`{}`), map[string]string{
			"Cookie": SessionCookieName + "=" + res.Token, CSRFHeader: res.CSRFToken, "Content-Type": "application/json",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a session: %d; want 401", target.method, target.path, rec.Code)
		}
	}
}

// TestUnknownRoutesNeedCredentials checks that paths no route matches, under /api/,
// still require credentials instead of falling through unauthenticated.
func TestUnknownRoutesNeedCredentials(t *testing.T) {
	f := newScopeFixture(t)
	for _, path := range []string{"/api/v1/nope", "/api/v2/jobs", "/api/v1/jobs/x/y/z", "/api/v1//jobs", "/api/v1/JOBS"} {
		if rec := serve(f.h, "GET", path, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials: %d; want 401", path, rec.Code)
		}
	}
	// A path-cleaning redirect must not leak data either: /api/v1/./jobs is refused
	// or redirected, never served.
	if rec := serve(f.h, "GET", "/api/v1/./jobs", nil, nil); rec.Code == http.StatusOK {
		t.Errorf("GET /api/v1/./jobs without credentials: 200")
	}
}

// TestPublicRoutesAreExactlyTheseFour pins the unauthenticated API surface.
func TestPublicRoutesAreExactlyTheseFour(t *testing.T) {
	want := []string{"/api/v1/auth/login", "/api/v1/health", "/api/v1/setup", "/api/v1/setup/status"}
	if len(publicPaths) != len(want) {
		t.Fatalf("publicPaths = %v; want exactly %v", publicPaths, want)
	}
	for _, p := range want {
		if !publicPaths[p] {
			t.Errorf("publicPaths lacks %s", p)
		}
	}
	f := newScopeFixture(t)
	for _, p := range f.srv.patterns {
		if _, ok := routeScopes[p]; ok {
			_, path := concrete(p)
			if publicPaths[path] {
				t.Errorf("%s is both public and scoped", p)
			}
		}
	}
}
