package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// testClock is a manually advanced time source for the auth service.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// hardeningFixture is a full server whose auth service runs on a manual clock and a
// store in a known directory, so tests can expire sessions and inspect the files at
// rest.
type hardeningFixture struct {
	*authFixture
	clock *testClock
	store *store.SQLiteStore
	dir   string
}

func newHardeningFixture(t *testing.T, mutate func(*testConfig), opts ...auth.Option) *hardeningFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	dir := t.TempDir()
	st := storetest.Open(t, filepath.Join(dir, "mongorescue.db"))
	cfg := newTestConfig()
	if mutate != nil {
		mutate(cfg)
	}
	clk := &testClock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	svc := newTestAuth(t, st, "", append([]auth.Option{auth.WithClock(clk.Now)}, opts...)...)
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, cfg.Security)))
	return &hardeningFixture{authFixture: &authFixture{h: full.Handler(), auth: svc}, clock: clk, store: st, dir: dir}
}

// TestLoginLockoutIsPerUserAndPerAddress checks the throttling policy over HTTP: the
// failures of one (address, username) pair lock only that pair, the lockout ignores
// the username's case, unknown usernames lock exactly like real ones (no user
// enumeration), and forwarded addresses count only behind a trusted proxy.
func TestLoginLockoutIsPerUserAndPerAddress(t *testing.T) {
	const attacker, other = "203.0.113.5", "198.51.100.9"
	for _, tc := range []struct {
		name     string
		trust    bool
		ip       string
		headers  map[string]string
		user     string
		password string
		want     int
	}{
		{"locked pair, right password", false, attacker, nil, "admin", testPassword, http.StatusTooManyRequests},
		{"locked pair, other case", false, attacker, nil, "ADMIN", testPassword, http.StatusTooManyRequests},
		{"same address, other user", false, attacker, nil, "bob", testPassword, http.StatusOK},
		{"other address, same user", false, other, nil, "admin", testPassword, http.StatusOK},
		{"spoofed forwarding header", false, attacker, map[string]string{"X-Forwarded-For": other}, "admin", testPassword, http.StatusTooManyRequests},
		{"trusted proxy, other client", true, "10.0.0.2", map[string]string{"X-Forwarded-For": other}, "admin", testPassword, http.StatusOK},
		{"trusted proxy, locked client", true, "10.0.0.2", map[string]string{"X-Forwarded-For": other + ", " + attacker}, "admin", testPassword, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHardeningFixture(t, func(c *testConfig) { c.Security.TrustProxyHeaders = tc.trust })
			admin := f.browser(t)
			admin.setup(f.authFixture)
			if rec := admin.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": testPassword}, nil); rec.Code != http.StatusCreated {
				t.Fatalf("create bob: %d", rec.Code)
			}
			// The attacker's address fails FreeAttempts times for admin, via the proxy
			// when there is one.
			a := f.browser(t)
			a.ip = attacker
			var hdr map[string]string
			if tc.trust {
				a.ip = "10.0.0.2"
				hdr = map[string]string{"X-Forwarded-For": attacker}
			}
			for range auth.FreeAttempts {
				if rec := a.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong password"}, hdr); rec.Code != http.StatusUnauthorized {
					t.Fatalf("failure: %d", rec.Code)
				}
			}
			c := f.browser(t)
			c.ip = tc.ip
			rec := c.do("POST", "/api/v1/auth/login", map[string]string{"username": tc.user, "password": tc.password}, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("login: %d %s; want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
				t.Fatal("a lockout must carry Retry-After")
			}
			// The lockout ends: after MaxLockout the right password works again.
			f.clock.Advance(auth.MaxLockout)
			if rec := c.do("POST", "/api/v1/auth/login", map[string]string{"username": tc.user, "password": tc.password}, tc.headers); rec.Code != http.StatusOK {
				t.Fatalf("login after the lockout: %d", rec.Code)
			}
		})
	}
}

// TestUnknownUsersLockOutLikeRealOnes checks that the lockout does not reveal which
// usernames exist: the responses for a real and an unknown user are identical before
// and after the lockout.
func TestUnknownUsersLockOutLikeRealOnes(t *testing.T) {
	f := newHardeningFixture(t, nil)
	f.browser(t).setup(f.authFixture)
	responses := map[string][]string{}
	for _, user := range []string{"admin", "ghost"} {
		b := f.browser(t)
		for range auth.FreeAttempts + 1 {
			rec := b.do("POST", "/api/v1/auth/login", map[string]string{"username": user, "password": "wrong password"}, nil)
			responses[user] = append(responses[user], rec.Result().Status+" "+rec.Body.String())
		}
	}
	if strings.Join(responses["admin"], "\n") != strings.Join(responses["ghost"], "\n") {
		t.Fatalf("real and unknown users are distinguishable:\n%v\n%v", responses["admin"], responses["ghost"])
	}
	if last := responses["ghost"][auth.FreeAttempts]; !strings.HasPrefix(last, "429") {
		t.Fatalf("attempt %d: %s; want 429", auth.FreeAttempts+1, last)
	}
}

// TestSessionsExpireOverHTTP checks the idle and absolute session lifetimes end to
// end: an expired session is refused with 401 and its cookie cleared, and continuous
// activity does not extend a session past its absolute lifetime.
func TestSessionsExpireOverHTTP(t *testing.T) {
	const idle, absolute = 30 * time.Minute, 2 * time.Hour
	f := newHardeningFixture(t, nil, auth.WithSessionTimeouts(idle, absolute))
	b := f.browser(t)
	b.setup(f.authFixture)

	f.clock.Advance(idle - time.Minute)
	if rec := b.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("active session: %d", rec.Code)
	}
	stolen := b.cookie
	f.clock.Advance(idle)
	rec := b.do("GET", "/api/v1/jobs", nil, nil)
	if rec.Code != http.StatusUnauthorized || sessionCookie(t, rec).MaxAge >= 0 {
		t.Fatalf("idle session: %d; want 401 and a cleared cookie", rec.Code)
	}
	replay := f.browser(t)
	replay.cookie = stolen
	f.clock.Advance(-idle) // turning the clock back does not revive a deleted session
	if rec := replay.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session replayed: %d; want 401", rec.Code)
	}
	f.clock.Advance(idle)

	// Absolute lifetime: a request every few minutes does not keep a session alive.
	b.session(b.login("admin", testPassword))
	for elapsed := time.Duration(0); elapsed < absolute-10*time.Minute; elapsed += 10 * time.Minute {
		f.clock.Advance(10 * time.Minute)
		if rec := b.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("session at %v: %d", elapsed, rec.Code)
		}
	}
	f.clock.Advance(10 * time.Minute)
	if rec := b.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session past its absolute lifetime: %d; want 401", rec.Code)
	}
}

// TestSessionEndsWithItsUser checks that deleting a user and resetting a password
// revoke the user's sessions and API keys at once.
func TestSessionEndsWithItsUser(t *testing.T) {
	f := newHardeningFixture(t, nil)
	admin := f.browser(t)
	admin.setup(f.authFixture)
	var bob auth.User
	decodeData(t, admin.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": testPassword}, nil), &bob)
	bobSession := f.browser(t)
	bobSession.session(bobSession.login("bob", testPassword))
	var key createdAPIKey
	decodeData(t, bobSession.do("POST", "/api/v1/api-keys", map[string]string{"name": "bob's", "scope": "admin"}, nil), &key)
	if key.Key == "" {
		t.Fatal("bob could not create a key")
	}
	if rec := admin.do("DELETE", "/api/v1/users/"+bob.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete bob: %d", rec.Code)
	}
	if rec := bobSession.do("GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's session: %d; want 401", rec.Code)
	}
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"X-API-Key": key.Key}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's API key: %d; want 401", rec.Code)
	}
}

// TestSessionCookieFlagsOnEveryIssue checks the flags of every session cookie the
// server sets (setup, login and the clearing cookie of logout), with and without TLS.
func TestSessionCookieFlagsOnEveryIssue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trust   bool
		tls     bool
		headers map[string]string
		secure  bool
	}{
		{"plain http", false, false, nil, false},
		{"direct tls", false, true, nil, true},
		{"trusted https proxy", true, false, map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"untrusted https header", false, false, map[string]string{"X-Forwarded-Proto": "https"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHardeningFixture(t, func(c *testConfig) { c.Security.TrustProxyHeaders = tc.trust })
			do := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if tc.tls {
					req.TLS = &tls.ConnectionState{}
				}
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}
				if cookie != nil {
					req.AddCookie(cookie)
					req.Header.Set(CSRFHeader, csrf)
				}
				rec := httptest.NewRecorder()
				f.h.ServeHTTP(rec, req)
				return rec
			}
			check := func(what string, rec *httptest.ResponseRecorder, cleared bool) *http.Cookie {
				t.Helper()
				c := sessionCookie(t, rec)
				if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure != tc.secure || c.Domain != "" {
					t.Errorf("%s cookie = %+v; want HttpOnly, SameSite=Strict, Path=/, host-only, Secure=%v", what, c, tc.secure)
				}
				if cleared != (c.MaxAge < 0) {
					t.Errorf("%s cookie MaxAge = %d", what, c.MaxAge)
				}
				if !cleared && (len(c.Value) < 32 || strings.Contains(rec.Body.String(), c.Value)) {
					t.Errorf("%s: the session token must be long and only in the cookie", what)
				}
				return c
			}
			setup := do("POST", "/api/v1/setup", `{"setup_code":"`+f.auth.SetupCode()+`","username":"admin","password":"`+testPassword+`"}`, nil, "")
			check("setup", setup, false)
			login := do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+testPassword+`"}`, nil, "")
			c := check("login", login, false)
			var res sessionResponse
			decodeData(t, login, &res)
			check("logout", do("POST", "/api/v1/auth/logout", `{}`, c, res.CSRFToken), true)
		})
	}
}

// TestSetupModeCannotBeReentered checks that once the first admin exists, setup is
// refused with the old code, after a restart, and that the last user (whose removal
// would reopen setup) cannot be deleted.
func TestSetupModeCannotBeReentered(t *testing.T) {
	f := newHardeningFixture(t, nil)
	code := f.auth.SetupCode()
	admin := f.browser(t)
	me := admin.setup(f.authFixture)
	if f.auth.SetupCode() != "" {
		t.Fatal("the setup code must be discarded after setup")
	}
	attempt := func(h http.Handler, code string) int {
		return serve(h, "POST", "/api/v1/setup", []byte(`{"setup_code":"`+code+`","username":"mallory","password":"`+testPassword+`"}`),
			map[string]string{"Content-Type": "application/json"}).Code
	}
	for _, c := range []string{code, "", "WRONG"} {
		if got := attempt(f.h, c); got != http.StatusConflict && got != http.StatusForbidden {
			t.Errorf("setup with code %q after setup: %d; want 409 or 403", c, got)
		}
	}

	// A restart on the same database stays out of setup mode.
	restarted := newTestAuth(t, f.store, "")
	if restarted.SetupCode() != "" {
		t.Fatal("a restart after setup must not print a new setup code")
	}
	if required, err := restarted.SetupRequired(context.Background()); err != nil || required {
		t.Fatalf("SetupRequired after restart = %v, %v", required, err)
	}
	srv, _, _ := setupTestServer(t)
	h := NewServer(bootConfig(), f.store, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(restarted), WithSettings(newTestSettings(t, f.store, settings.Defaults().Security))).Handler()
	if got := attempt(h, code); got != http.StatusConflict {
		t.Errorf("setup after restart: %d; want 409", got)
	}
	if rec := admin.do("DELETE", "/api/v1/users/"+me.User.ID, nil, nil); rec.Code == http.StatusOK {
		t.Fatal("the last user must not be deletable")
	}
	users, err := f.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("users = %v, %v; want only admin", users, err)
	}
}

// TestAPIKeysAreStoredOnlyAsHashes creates a key over HTTP and checks that its
// plaintext is returned once, never listed, stored only as its SHA-256 hash (not in
// the database files at all) and refused once revoked.
func TestAPIKeysAreStoredOnlyAsHashes(t *testing.T) {
	f := newHardeningFixture(t, nil)
	admin := f.browser(t)
	admin.setup(f.authFixture)
	var created createdAPIKey
	decodeData(t, admin.do("POST", "/api/v1/api-keys", map[string]string{"name": "ci", "scope": "operator"}, nil), &created)
	plain := created.Key
	if len(plain) < 40 {
		t.Fatalf("key %q is too short", plain)
	}
	stored, err := f.store.GetAPIKeyByPrefix(context.Background(), created.APIKey.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Hash != auth.HashToken(plain) || strings.Contains(stored.Hash, plain) {
		t.Fatalf("stored hash %q is not SHA-256 of the key", stored.Hash)
	}
	if list := admin.do("GET", "/api/v1/api-keys", nil, nil); strings.Contains(list.Body.String(), plain[len("mr_")+9:]) {
		t.Fatalf("listing shows the key: %s", list.Body)
	}
	secret := []byte(plain[len("mr_")+9:])
	files, _ := filepath.Glob(filepath.Join(f.dir, "mongorescue.db*"))
	if len(files) == 0 {
		t.Fatal("no database files found")
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, secret) {
			t.Fatalf("%s contains the plaintext API key", filepath.Base(name))
		}
	}

	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Authorization": "Bearer " + plain}); rec.Code != http.StatusOK {
		t.Fatalf("key before revocation: %d", rec.Code)
	}
	if rec := admin.do("DELETE", "/api/v1/api-keys/"+created.APIKey.ID, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	for _, hdr := range []map[string]string{{"Authorization": "Bearer " + plain}, {"X-API-Key": plain}} {
		if rec := serve(f.h, "GET", "/api/v1/jobs", nil, hdr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked key via %v: %d; want 401", hdr, rec.Code)
		}
	}
	// The hash itself is not a credential.
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"X-API-Key": stored.Hash}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hash as a key: %d; want 401", rec.Code)
	}
}
