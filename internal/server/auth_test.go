package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

const testPassword = "a long enough password"

// authFixture is a full server (middleware chain) with real auth and connections.
type authFixture struct {
	h    http.Handler
	auth *auth.Service
}

func newAuthFixture(t *testing.T, mutate func(*testConfig)) *authFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	cfg := newTestConfig()
	if mutate != nil {
		mutate(cfg)
	}
	svc := newTestAuth(t, st, cfg.APIKey)
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, cfg.Security)))
	return &authFixture{h: full.Handler(), auth: svc}
}

// browser keeps a session cookie and CSRF token like the dashboard does.
type browser struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	csrf   string
	ip     string
}

func (f *authFixture) browser(t *testing.T) *browser {
	return &browser{t: t, h: f.h, ip: "192.0.2.1"}
}

func (b *browser) do(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = b.ip + ":40000"
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf != "" {
		req.Header.Set(CSRFHeader, b.csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			if c.MaxAge < 0 {
				b.cookie = nil
			} else {
				b.cookie = c
			}
		}
	}
	return rec
}

// session decodes a setup/login response and adopts its CSRF token.
func (b *browser) session(rec *httptest.ResponseRecorder) sessionResponse {
	b.t.Helper()
	var res sessionResponse
	decodeData(b.t, rec, &res)
	b.csrf = res.CSRFToken
	return res
}

func (b *browser) setup(f *authFixture) sessionResponse {
	b.t.Helper()
	rec := b.do("POST", "/api/v1/setup", map[string]string{"setup_code": f.auth.SetupCode(), "username": "admin", "password": testPassword}, nil)
	if rec.Code != http.StatusCreated {
		b.t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	return b.session(rec)
}

func (b *browser) login(user, password string) *httptest.ResponseRecorder {
	b.t.Helper()
	b.csrf = ""
	return b.do("POST", "/api/v1/auth/login", map[string]string{"username": user, "password": password}, nil)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", SessionCookieName)
	return nil
}

func TestSetupFlowOverHTTP(t *testing.T) {
	f := newAuthFixture(t, nil)
	b := f.browser(t)

	var status map[string]bool
	decodeData(t, b.do("GET", "/api/v1/setup/status", nil, nil), &status)
	if !status["setup_required"] {
		t.Fatal("setup_required must be true on an empty database")
	}
	if rec := b.do("POST", "/api/v1/setup", map[string]string{"setup_code": "WRONG", "username": "admin", "password": testPassword}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong setup code: %d", rec.Code)
	}
	if rec := b.do("POST", "/api/v1/setup", "not an object", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d", rec.Code)
	}
	if rec := b.do("POST", "/api/v1/setup", map[string]string{"setup_code": f.auth.SetupCode(), "username": "admin", "password": "short"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("weak password: %d", rec.Code)
	}

	code := f.auth.SetupCode()
	rec := b.do("POST", "/api/v1/setup", map[string]string{"setup_code": code, "username": "admin", "password": testPassword}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "$2a$") || strings.Contains(rec.Body.String(), testPassword) {
		t.Fatalf("setup response leaks credentials: %s", rec.Body.String())
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure || c.MaxAge <= 0 {
		t.Fatalf("cookie flags = %+v", c)
	}
	res := b.session(rec)
	if res.User == nil || res.User.Username != "admin" || res.CSRFToken == "" {
		t.Fatalf("setup response = %+v", res)
	}

	if rec = b.do("POST", "/api/v1/setup", map[string]string{"setup_code": code, "username": "x", "password": testPassword}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("second setup: %d; want 409", rec.Code)
	}
	decodeData(t, b.do("GET", "/api/v1/setup/status", nil, nil), &status)
	if status["setup_required"] {
		t.Fatal("setup_required must be false after setup")
	}

	var me sessionResponse
	decodeData(t, b.do("GET", "/api/v1/auth/me", nil, nil), &me)
	if me.Auth != auth.MethodSession || me.User.Username != "admin" || me.CSRFToken != res.CSRFToken {
		t.Fatalf("me = %+v", me)
	}
	anon := f.browser(t)
	rec = anon.do("GET", "/api/v1/auth/me", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":{"user":null,"csrf_token":"","auth":""}`) {
		t.Fatalf("me without session: %d %s; want 200 with an empty session", rec.Code, rec.Body)
	}
	// A stale cookie is cleared and reported as signed out; other routes stay 401.
	rec = anon.do("GET", "/api/v1/auth/me", nil, map[string]string{"Cookie": SessionCookieName + "=stale"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"user":null`) {
		t.Fatalf("me with a stale cookie: %d %s", rec.Code, rec.Body)
	}
	if rec := anon.do("GET", "/api/v1/users", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("users without session: %d; want 401", rec.Code)
	}
	if rec := anon.do("POST", "/api/v1/auth/me", nil, nil); rec.Code == http.StatusOK {
		t.Fatal("only GET /api/v1/auth/me is open to signed-out visitors")
	}
}

func TestSecureCookieFlag(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*testConfig)
		headers map[string]string
		tls     bool
		secure  bool
	}{
		{"plain http", nil, nil, false, false},
		{"tls", nil, nil, true, true},
		{"forced", func(c *testConfig) { c.Security.SecureCookies = settings.CookiesAlways }, nil, false, true},
		{"never", func(c *testConfig) { c.Security.SecureCookies = settings.CookiesNever }, nil, true, false},
		{"trusted proxy https", func(c *testConfig) { c.Security.TrustProxyHeaders = true }, map[string]string{"X-Forwarded-Proto": "https"}, false, true},
		{"untrusted proxy header", nil, map[string]string{"X-Forwarded-Proto": "https"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t, tc.mutate)
			body, _ := json.Marshal(map[string]string{"setup_code": f.auth.SetupCode(), "username": "admin", "password": testPassword})
			req := httptest.NewRequest("POST", "/api/v1/setup", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			f.h.ServeHTTP(rec, req)
			if c := sessionCookie(t, rec); c.Secure != tc.secure {
				t.Fatalf("Secure = %v; want %v", c.Secure, tc.secure)
			}
		})
	}
}

func TestLoginOverHTTPIsGenericAndThrottled(t *testing.T) {
	f := newAuthFixture(t, nil)
	f.browser(t).setup(f)

	b := f.browser(t)
	unknown := b.login("nobody", testPassword)
	wrong := b.login("admin", "not the password")
	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized || unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("login failures must be identical 401s: %d %s / %d %s", unknown.Code, unknown.Body, wrong.Code, wrong.Body)
	}
	for range auth.FreeAttempts - 1 {
		b.login("admin", "not the password")
	}
	// A spoofed X-Forwarded-For is ignored without trust_proxy_headers.
	rec := b.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword},
		map[string]string{"X-Forwarded-For": "203.0.113.99"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("locked login: %d (Retry-After %q); want 429", rec.Code, rec.Header().Get("Retry-After"))
	}

	other := f.browser(t)
	other.ip = "198.51.100.20"
	ok := other.login("admin", testPassword)
	if ok.Code != http.StatusOK {
		t.Fatalf("login from another address: %d", ok.Code)
	}
	if res := other.session(ok); res.User == nil || res.CSRFToken == "" {
		t.Fatalf("login response = %+v", res)
	}
}

func TestClientIPHonoursProxyHeadersOnlyWhenTrusted(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.4")
	untrusted := &Server{cfg: bootConfig()}
	if got := untrusted.clientIP(req); got != "10.0.0.2" {
		t.Fatalf("untrusted clientIP = %q", got)
	}
	sec := settings.Defaults().Security
	sec.TrustProxyHeaders = true
	trusted := &Server{cfg: bootConfig(), settings: newTestSettings(t, storetest.New(t), sec)}
	if got := trusted.clientIP(req); got != "198.51.100.4" {
		t.Fatalf("trusted clientIP = %q; want the proxy-appended entry", got)
	}
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "203.0.113.8")
	if got := trusted.clientIP(req); got != "203.0.113.8" {
		t.Fatalf("X-Real-IP = %q", got)
	}
}

func TestCSRFRequiredForCookieNotForAPIKey(t *testing.T) {
	const static = "csrf-test-static-key"
	f := newAuthFixture(t, func(c *testConfig) { c.APIKey = static })
	b := f.browser(t)
	b.setup(f)
	token := b.csrf
	conn := map[string]string{"name": "db", "uri": "mongodb://h:27017/"}

	b.csrf = ""
	if rec := b.do("POST", "/api/v1/connections", conn, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF: %d; want 403", rec.Code)
	}
	if rec := b.do("DELETE", "/api/v1/connections/"+testConnID, nil, map[string]string{CSRFHeader: "wrong"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie DELETE with wrong CSRF: %d; want 403", rec.Code)
	}
	if rec := b.do("GET", "/api/v1/connections", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET needs no CSRF: %d", rec.Code)
	}
	b.csrf = token
	if rec := b.do("POST", "/api/v1/connections", conn, nil); rec.Code != http.StatusCreated {
		t.Fatalf("cookie POST with CSRF: %d %s", rec.Code, rec.Body.String())
	}

	api := f.browser(t)
	if rec := api.do("POST", "/api/v1/connections", conn, map[string]string{"Authorization": "Bearer " + static}); rec.Code != http.StatusCreated {
		t.Fatalf("API key POST without CSRF: %d %s", rec.Code, rec.Body.String())
	}
	var me sessionResponse
	decodeData(t, api.do("GET", "/api/v1/auth/me", nil, map[string]string{"X-API-Key": static}), &me)
	if me.Auth != auth.MethodAPIKey || me.User != nil || me.CSRFToken != "" {
		t.Fatalf("me via static key = %+v", me)
	}
}

func TestLogoutRevokesSessionOverHTTP(t *testing.T) {
	f := newAuthFixture(t, nil)
	b := f.browser(t)
	b.setup(f)
	stolen := b.cookie

	rec := b.do("POST", "/api/v1/auth/logout", nil, nil)
	if rec.Code != http.StatusOK || sessionCookie(t, rec).MaxAge >= 0 {
		t.Fatalf("logout: %d, cookie must be cleared", rec.Code)
	}
	replay := f.browser(t)
	replay.cookie = stolen
	if rec := replay.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session cookie: %d; want 401", rec.Code)
	}
}

func TestUsersAPI(t *testing.T) {
	f := newAuthFixture(t, nil)
	admin := f.browser(t)
	me := admin.setup(f)

	rec := admin.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": testPassword}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	var bob auth.User
	decodeData(t, rec, &bob)
	if rec := admin.do("POST", "/api/v1/users", map[string]string{"username": "BOB", "password": testPassword}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate user: %d; want 409", rec.Code)
	}
	var users []map[string]any
	decodeData(t, admin.do("GET", "/api/v1/users", nil, nil), &users)
	if len(users) != 2 {
		t.Fatalf("users = %v", users)
	}
	for _, u := range users {
		for k := range u {
			if k != "id" && k != "username" && k != "created_at" && k != "last_login_at" {
				t.Errorf("unexpected user field %q", k)
			}
		}
	}

	// Bob logs in twice; the admin resets his password: both sessions end.
	bob1, bob2 := f.browser(t), f.browser(t)
	bob1.session(bob1.login("bob", testPassword))
	bob2.session(bob2.login("bob", testPassword))
	if rec := admin.do("PUT", "/api/v1/users/"+bob.ID+"/password", map[string]string{"new_password": "reset by the admin!"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("admin reset: %d %s", rec.Code, rec.Body.String())
	}
	for _, s := range []*browser{bob1, bob2} {
		if rec := s.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bob's session after reset: %d", rec.Code)
		}
	}

	// Own password: the current one is required; other sessions are revoked, this one stays.
	second := f.browser(t)
	second.session(second.login("admin", testPassword))
	path := "/api/v1/users/" + me.User.ID + "/password"
	if rec := admin.do("PUT", path, map[string]string{"new_password": "brand new password"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("own change without current password: %d; want 403", rec.Code)
	}
	if rec := admin.do("PUT", path, map[string]string{"current_password": testPassword, "new_password": "brand new password"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("own change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := admin.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("acting session after own change: %d", rec.Code)
	}
	if rec := second.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("other session after own change: %d", rec.Code)
	}

	if rec := admin.do("DELETE", "/api/v1/users/"+me.User.ID, nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete self: %d; want 400", rec.Code)
	}
	if rec := admin.do("DELETE", "/api/v1/users/"+bob.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete bob: %d", rec.Code)
	}
	if rec := admin.do("DELETE", "/api/v1/users/"+bob.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete bob twice: %d; want 404", rec.Code)
	}
}

func TestLastUserCannotBeDeletedOverHTTP(t *testing.T) {
	const static = "last-user-static-key"
	f := newAuthFixture(t, func(c *testConfig) { c.APIKey = static })
	me := f.browser(t).setup(f)
	api := f.browser(t)
	if rec := api.do("DELETE", "/api/v1/users/"+me.User.ID, nil, map[string]string{"X-API-Key": static}); rec.Code != http.StatusConflict {
		t.Fatalf("delete last user: %d; want 409", rec.Code)
	}
}

func TestAPIKeysAPI(t *testing.T) {
	f := newAuthFixture(t, nil)
	admin := f.browser(t)
	admin.setup(f)

	if rec := admin.do("POST", "/api/v1/api-keys", map[string]string{"name": ""}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty key name: %d", rec.Code)
	}
	rec := admin.do("POST", "/api/v1/api-keys", map[string]string{"name": "ci"}, nil)
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create key: %d (Cache-Control %q)", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var created createdAPIKey
	decodeData(t, rec, &created)
	if !strings.HasPrefix(created.Key, "mr_"+created.APIKey.Prefix+"_") || len(created.APIKey.Prefix) != 8 {
		t.Fatalf("created = %+v", created)
	}

	list := admin.do("GET", "/api/v1/api-keys", nil, nil)
	secret := created.Key[len("mr_")+9:]
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), secret) || strings.Contains(list.Body.String(), auth.HashToken(created.Key)) {
		t.Fatalf("key listing leaks key material: %s", list.Body.String())
	}
	var keys []map[string]any
	decodeData(t, list, &keys)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	for _, field := range []string{"id", "name", "prefix", "created_by", "created_at"} {
		if _, ok := keys[0][field]; !ok {
			t.Errorf("api key item lacks %q: %v", field, keys[0])
		}
	}

	ci := f.browser(t)
	if rec := ci.do("GET", "/api/v1/jobs", nil, map[string]string{"Authorization": "Bearer " + created.Key}); rec.Code != http.StatusOK {
		t.Fatalf("request with the new key: %d", rec.Code)
	}
	decodeData(t, admin.do("GET", "/api/v1/api-keys", nil, nil), &keys)
	if keys[0]["last_used_at"] == nil {
		t.Fatal("last_used_at must be set after use")
	}
	if rec := admin.do("DELETE", "/api/v1/api-keys/"+created.APIKey.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := ci.do("GET", "/api/v1/jobs", nil, map[string]string{"X-API-Key": created.Key}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d; want 401", rec.Code)
	}
	if rec := admin.do("DELETE", "/api/v1/api-keys/"+created.APIKey.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke twice: %d; want 404", rec.Code)
	}
}

func TestSetupURL(t *testing.T) {
	for host, want := range map[string]string{
		"":           "http://localhost:8080/",
		"0.0.0.0":    "http://localhost:8080/",
		"::":         "http://localhost:8080/",
		"127.0.0.1":  "http://127.0.0.1:8080/",
		"::1":        "http://[::1]:8080/",
		"[::1]":      "http://[::1]:8080/",
		"backup.lan": "http://backup.lan:8080/",
	} {
		if got := setupURL(host, 8080); got != want {
			t.Errorf("setupURL(%q) = %q; want %q", host, got, want)
		}
	}
}

func TestPublicPostsRejectCrossSiteRequests(t *testing.T) {
	f := newAuthFixture(t, func(c *testConfig) { c.Security.CORSOrigins = []string{"https://ops.example.com"} })
	f.browser(t).setup(f)
	login := map[string]string{"username": "admin", "password": testPassword}
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"text/plain form post", "/api/v1/auth/login", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"urlencoded form post", "/api/v1/auth/login", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"no content type", "/api/v1/auth/login", map[string]string{"Content-Type": ""}, http.StatusUnsupportedMediaType},
		{"setup as text/plain", "/api/v1/setup", map[string]string{"Content-Type": "text/plain;charset=UTF-8"}, http.StatusUnsupportedMediaType},
		{"foreign origin", "/api/v1/auth/login", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null origin", "/api/v1/auth/login", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"foreign origin on setup", "/api/v1/setup", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same origin", "/api/v1/auth/login", map[string]string{"Origin": "http://example.com"}, http.StatusOK},
		{"same origin, json with charset", "/api/v1/auth/login", map[string]string{"Origin": "https://EXAMPLE.com", "Content-Type": "application/json; charset=utf-8"}, http.StatusOK},
		{"configured cors origin", "/api/v1/auth/login", map[string]string{"Origin": "https://ops.example.com"}, http.StatusOK},
		{"no origin (curl)", "/api/v1/auth/login", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := f.browser(t)
			b.ip = "198.51.100.30" // keep throttling out of the picture
			rec := b.do("POST", tc.path, login, tc.headers)
			if tc.want == http.StatusOK && tc.path == "/api/v1/setup" {
				tc.want = http.StatusConflict
			}
			if rec.Code != tc.want {
				t.Fatalf("POST %s: %d %s; want %d", tc.path, rec.Code, rec.Body, tc.want)
			}
			if tc.want != http.StatusOK && len(rec.Result().Cookies()) != 0 {
				t.Fatal("a refused request must not set a session cookie")
			}
		})
	}
}
