package desktop

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/server"
)

// cookieServer sets the session cookie on /login, clears it on /logout and echoes
// the received session cookie in the X-Got header everywhere.
func cookieServer(maxAge int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(server.SessionCookieName); err == nil {
			w.Header().Set("X-Got", c.Value)
		}
		switch r.URL.Path {
		case "/login":
			w.Header().Add("Set-Cookie", fmt.Sprintf("%s=tok1; Path=/; Max-Age=%d; HttpOnly; SameSite=Strict", server.SessionCookieName, maxAge))
			w.Header().Add("Set-Cookie", "other=x")
		case "/logout":
			w.Header().Add("Set-Cookie", server.SessionCookieName+"=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}

// serve sends GET path to h, with cookie ("name=value") as the Cookie header unless
// it is empty.
func serve(h http.Handler, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSessionJarCapturesInjectsAndClears(t *testing.T) {
	jar := NewSessionJar(server.SessionCookieName)
	h := jar.Wrap(cookieServer(3600))

	if got := serve(h, "/me", "").Header().Get("X-Got"); got != "" {
		t.Fatalf("empty jar injected %q", got)
	}
	rec := serve(h, "/login", "")
	if rec.Header().Get("Set-Cookie") == "" {
		t.Fatal("Set-Cookie must still reach the webview")
	}
	if jar.Value() != "tok1" {
		t.Fatalf("jar = %q; want tok1", jar.Value())
	}
	if got := serve(h, "/me", "").Header().Get("X-Got"); got != "tok1" {
		t.Fatalf("injected cookie = %q; want tok1", got)
	}
	// A request that already carries the cookie is left alone.
	if got := serve(h, "/me", server.SessionCookieName+"=own").Header().Get("X-Got"); got != "own" {
		t.Fatalf("request cookie overwritten: %q", got)
	}
	// An unrelated cookie does not count as carrying the session cookie.
	if got := serve(h, "/me", "other=x").Header().Get("X-Got"); got != "tok1" {
		t.Fatalf("with unrelated cookie = %q; want tok1", got)
	}
	serve(h, "/logout", "")
	if jar.Value() != "" {
		t.Fatalf("jar after logout = %q", jar.Value())
	}
	if got := serve(h, "/me", "").Header().Get("X-Got"); got != "" {
		t.Fatalf("cleared cookie injected: %q", got)
	}
}

func TestSessionJarExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jar := NewSessionJar(server.SessionCookieName)
	jar.now = func() time.Time { return now }
	h := jar.Wrap(cookieServer(60))

	serve(h, "/login", "")
	if jar.Value() != "tok1" {
		t.Fatalf("jar = %q", jar.Value())
	}
	now = now.Add(61 * time.Second)
	if got := serve(h, "/me", "").Header().Get("X-Got"); got != "" {
		t.Fatalf("expired cookie injected: %q", got)
	}

	// An Expires date in the past clears the jar too.
	serve(h, "/login", "")
	jar.record([]string{server.SessionCookieName + "=old; Expires=" + now.Add(-time.Hour).Format(http.TimeFormat)})
	if jar.Value() != "" {
		t.Fatalf("past Expires kept %q", jar.Value())
	}
	// Without MaxAge, a future Expires is honoured.
	jar.record([]string{server.SessionCookieName + "=tok2; Expires=" + now.Add(time.Hour).Format(http.TimeFormat)})
	if jar.Value() != "tok2" {
		t.Fatalf("future Expires = %q", jar.Value())
	}
	now = now.Add(2 * time.Hour)
	if jar.Value() != "" {
		t.Fatal("cookie outlived its Expires")
	}
}

func TestSessionJarConcurrent(t *testing.T) {
	jar := NewSessionJar(server.SessionCookieName)
	h := jar.Wrap(cookieServer(3600))
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				serve(h, "/login", "")
			} else {
				serve(h, "/me", "")
			}
		}()
	}
	wg.Wait()
	if jar.Value() != "tok1" {
		t.Fatalf("jar = %q", jar.Value())
	}
}

func TestStripSameOrigin(t *testing.T) {
	h := StripSameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin", r.Header.Get("Origin"))
	}))
	for _, tc := range []struct{ host, origin, want string }{
		{"wails", "wails://wails", ""},
		{"wails.localhost", "http://wails.localhost", ""},
		{"wails", "https://evil.example", "https://evil.example"},
		{"wails", "null", "null"},
		{"wails", "", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.Host = tc.host
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Origin"); got != tc.want {
			t.Errorf("host %q origin %q: forwarded %q; want %q", tc.host, tc.origin, got, tc.want)
		}
		if tc.origin != "" && req.Header.Get("Origin") != tc.origin {
			t.Errorf("caller's request mutated")
		}
	}
}

func TestHandlerLogsInThroughTheRealServerOrigin(t *testing.T) {
	// Handler composes both adapters: an in-process login from wails://wails keeps
	// its session for the next request.
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin", http.StatusForbidden)
			return
		}
		if c, err := r.Cookie(server.SessionCookieName); err == nil {
			got = c.Value
		}
		if r.URL.Path == "/login" {
			w.Header().Add("Set-Cookie", server.SessionCookieName+"=tok; Max-Age=60")
		}
	})
	h := Handler(inner)
	for _, path := range []string{"/login", "/me"} {
		req := httptest.NewRequest(http.MethodPost, "wails://wails"+path, nil)
		req.Header.Set("Origin", "wails://wails")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	if got != "tok" {
		t.Fatalf("session cookie on second request = %q; want tok", got)
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		site, origin string
		want         bool
	}{
		{"", "", true},
		{"same-origin", "", true},
		{"none", "", true},
		{"", "wails://wails", true},
		{"same-origin", "http://wails", true},
		{"cross-site", "", false},
		{"same-site", "", false},
		{"", "https://evil.example", false},
		{"same-origin", "null", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "wails://wails/api/v1/auth/me", nil)
		if tc.site != "" {
			req.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if got := SameOrigin(req); got != tc.want {
			t.Errorf("SameOrigin(site %q, origin %q) = %v; want %v", tc.site, tc.origin, got, tc.want)
		}
	}
}

func TestSessionJarIgnoresCrossSiteRequests(t *testing.T) {
	jar := NewSessionJar(server.SessionCookieName)
	h := jar.Wrap(cookieServer(3600))
	send := func(path, site, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "wails://wails"+path, nil)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A cross-site response cannot plant a session in the jar.
	send("/login", "cross-site", "")
	send("/login", "", "https://evil.example")
	if jar.Value() != "" {
		t.Fatalf("cross-site Set-Cookie stored: %q", jar.Value())
	}

	send("/login", "same-origin", "wails://wails")
	if jar.Value() != "tok1" {
		t.Fatalf("same-origin login not stored: %q", jar.Value())
	}
	// Cross-site requests never receive it, nor clear it.
	for _, tc := range []struct{ site, origin string }{
		{"cross-site", ""}, {"same-site", ""}, {"", "https://evil.example"}, {"", "null"},
	} {
		if got := send("/me", tc.site, tc.origin).Header().Get("X-Got"); got != "" {
			t.Errorf("site %q origin %q received the session cookie", tc.site, tc.origin)
		}
	}
	send("/logout", "cross-site", "https://evil.example")
	if jar.Value() != "tok1" {
		t.Fatal("cross-site logout cleared the jar")
	}
	if got := send("/me", "same-origin", "").Header().Get("X-Got"); got != "tok1" {
		t.Fatalf("same-origin request = %q; want tok1", got)
	}
}
