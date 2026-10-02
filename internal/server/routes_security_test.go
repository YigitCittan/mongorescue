package server

import (
	"context"
	"net/http"
	"slices"
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

// wrongToken returns a token of the same length as token that differs from it in
// the first character, whatever that character is.
func wrongToken(token string) string {
	if strings.HasPrefix(token, "0") {
		return "1" + token[1:]
	}
	return "0" + token[1:]
}

// newSession signs the admin in and returns a fresh session cookie and its CSRF
// token, so a case that ends its session cannot affect the next one.
func newSession(t *testing.T, f *scopeFixture) (cookie, csrf string) {
	t.Helper()
	res, err := f.auth.Login(context.Background(), "192.0.2.1", "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return SessionCookieName + "=" + res.Token, res.CSRFToken
}

// TestRouteSecurityMatrix walks every registered route and checks the whole access
// matrix against routeScopes: no credentials, every scope below the required one, a
// session cookie without or with a wrong CSRF token, and the required scope. A route
// registered without an entry in routeScopes fails here, so the table cannot fall
// behind the router. Routes run in sorted order and every session case signs in
// afresh, so no route can end the session another one relies on.
func TestRouteSecurityMatrix(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	patterns := f.authenticatedPatterns()
	slices.Sort(patterns)
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
				for _, c := range []struct {
					name  string
					token func(csrf string) string
				}{
					{"no", func(string) string { return "" }},
					{"a wrong", wrongToken},
					{"a truncated", func(csrf string) string { return csrf[:8] }},
				} {
					name := c.name
					cookie, csrf := newSession(t, f)
					h := with(map[string]string{"Cookie": cookie})
					if token := c.token(csrf); token != "" {
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

			// 4. The required scope gets through the middleware. Self-service routes
			// need read here but apply finer rules in the auth service (pinned by
			// TestRolesAreEnforcedForEveryRoute); an admin key passes those.
			key := f.keys[need]
			if slices.Contains(selfServiceRoutes, p) {
				key = f.keys[auth.ScopeAdmin]
			}
			rec = serve(f.h, method, path, []byte(`{}`), with(map[string]string{"Authorization": "Bearer " + key}))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s key: %d %s; want the handler's answer", need, rec.Code, rec.Body)
			}
		})
	}
}

// TestLogoutRequiresCSRF checks that logout, like every unsafe cookie-authenticated
// request, is refused without the session's CSRF token and leaves the session valid,
// so a cross-site request cannot sign the user out.
func TestLogoutRequiresCSRF(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := newSession(t, f)
	for _, c := range []struct{ name, token string }{{"no", ""}, {"a wrong", wrongToken(csrf)}, {"a truncated", csrf[:8]}} {
		h := map[string]string{"Cookie": cookie, "Content-Type": "application/json"}
		if c.token != "" {
			h[CSRFHeader] = c.token
		}
		if rec := serve(f.h, "POST", "/api/v1/auth/logout", []byte(`{}`), h); rec.Code != http.StatusForbidden {
			t.Errorf("logout with %s CSRF token: %d %s; want 403", c.name, rec.Code, rec.Body)
		}
		if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Cookie": cookie}); rec.Code != http.StatusOK {
			t.Fatalf("after logout with %s CSRF token: %d %s; want the session still valid", c.name, rec.Code, rec.Body)
		}
	}
	rec := serve(f.h, "POST", "/api/v1/auth/logout", []byte(`{}`), map[string]string{
		"Cookie": cookie, CSRFHeader: csrf, "Content-Type": "application/json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("logout with the CSRF token: %d %s; want 200", rec.Code, rec.Body)
	}
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Cookie": cookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d; want 401", rec.Code)
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
