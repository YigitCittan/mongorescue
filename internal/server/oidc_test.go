package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc"
	"github.com/yigitcittan/mongorescue/internal/auth/oidc/oidctest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

const ssoRedirect = "https://backup.example.com/auth/oidc/callback"

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// manualClock is a clock tests move forward.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ssoFixture is a full server with single sign-on against the fake provider.
type ssoFixture struct {
	h        http.Handler
	srv      *Server
	auth     *auth.Service
	settings *settings.Service
	idp      *oidctest.Provider
	box      *secretbox.Box
	log      *auditlog.Service
	logs     *syncBuffer
	clock    *manualClock
	admin    *browser
}

func newSSOFixture(t *testing.T, enable bool, withOIDC bool) *ssoFixture {
	t.Helper()
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := &manualClock{now: time.Now()}
	setSvc := newTestSettings(t, st, newTestConfig().Security)
	svc := newTestAuth(t, st, "", auth.WithLogger(logger), auth.WithClock(clock.Now), auth.WithOIDCPolicy(func() auth.OIDCPolicy {
		o := setSvc.Current().OIDC
		p := auth.OIDCPolicy{Enabled: o.Enabled, LocalLogin: auth.LocalLogin(o.LocalLogin), DefaultRole: auth.Role(o.DefaultRole),
			AllowedEmailDomains: o.AllowedEmailDomains, AutoCreateUsers: o.AutoCreateUsers}
		for _, m := range o.RoleMappings {
			p.RoleMappings = append(p.RoleMappings, auth.RoleMapping{Group: m.Group, Role: auth.Role(m.Role)})
		}
		return p
	}))
	idp := oidctest.New(t, oidctest.Client{ID: "mongorescue", Secret: "client-secret-value", RedirectURL: ssoRedirect})
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	alog := auditlog.New(auditlog.Config{Repo: st, Logger: slog.New(slog.DiscardHandler)})
	opts := []Option{WithAuth(svc), withTestConnection(t, st, nil), WithSettings(setSvc), WithAuditLog(alog)}
	if withOIDC {
		opts = append(opts, WithOIDC(oidc.NewClient(&http.Client{Timeout: 5 * time.Second}, oidc.WithClock(clock.Now)), box))
	}
	full := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, logger, opts...)
	f := &ssoFixture{h: full.Handler(), srv: full, auth: svc, settings: setSvc, idp: idp, box: box, log: alog, logs: logs, clock: clock}
	f.admin = (&authFixture{h: f.h, auth: svc}).browser(t)
	f.admin.setup(&authFixture{h: f.h, auth: svc})
	if _, err := setSvc.Update(context.Background(), settings.Patch{OIDC: &settings.OIDCPatch{
		Enabled: ptrTo(enable), Issuer: ptrTo(idp.Issuer), ClientID: ptrTo("mongorescue"), ClientSecret: ptrTo("client-secret-value"),
		RedirectURL:  ptrTo(ssoRedirect),
		RoleMappings: &[]settings.OIDCRoleMapping{{Group: "backup-ops", Role: "operator"}, {Group: "backup-admins", Role: "admin"}},
	}}); err != nil {
		t.Fatal(err)
	}
	idp.SetClaims(map[string]any{"preferred_username": "jane", "email": "jane@corp.com", "email_verified": true, "groups": []string{"backup-ops"}})
	return f
}

// ssoBrowser follows the sign-in like a browser, keeping the flow and session
// cookies.
type ssoBrowser struct {
	t       *testing.T
	f       *ssoFixture
	flow    *http.Cookie
	session *http.Cookie
	tls     bool
}

func (f *ssoFixture) browser(t *testing.T) *ssoBrowser { return &ssoBrowser{t: t, f: f} }

func (b *ssoBrowser) get(target string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.0.2.50:40000"
	if b.tls {
		req.TLS = &tls.ConnectionState{}
	}
	if b.flow != nil && strings.HasPrefix(req.URL.Path, "/auth/oidc/") {
		req.AddCookie(b.flow)
	}
	if b.session != nil {
		req.AddCookie(b.session)
	}
	rec := httptest.NewRecorder()
	b.f.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case OIDCFlowCookieName:
			if c.MaxAge < 0 {
				b.flow = nil
			} else {
				b.flow = c
			}
		case SessionCookieName:
			b.session = c
		}
	}
	return rec
}

// start begins a sign-in and returns the provider's authorization URL.
func (b *ssoBrowser) start(returnTo string) string {
	b.t.Helper()
	target := oidcStartPath
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	rec := b.get(target)
	if rec.Code != http.StatusFound || b.flow == nil {
		b.t.Fatalf("start: %d %s, flow cookie %v", rec.Code, rec.Body, b.flow)
	}
	return rec.Header().Get("Location")
}

// callback visits the callback URL the provider redirected to.
func (b *ssoBrowser) callback(cb *url.URL) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.get(cb.Path + "?" + cb.RawQuery)
}

// login runs the whole flow and returns the callback's response.
func (b *ssoBrowser) login(returnTo string) (*httptest.ResponseRecorder, *url.URL) {
	b.t.Helper()
	cb := b.f.idp.Authorize(b.t, b.start(returnTo))
	return b.callback(cb), cb
}

// failure returns the oidc_error code of a callback redirect ("" for none).
func failure(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: %d %s; want a redirect", rec.Code, rec.Body)
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("oidc_error")
}

func (b *ssoBrowser) me() meResponse {
	b.t.Helper()
	var me meResponse
	decodeData(b.t, b.get("/api/v1/auth/me"), &me)
	return me
}

func (f *ssoFixture) events(t *testing.T, action string) []*auditlog.Event {
	t.Helper()
	f.log.Flush()
	var out []*auditlog.Event
	if err := f.log.Export(context.Background(), auditlog.Filter{}, func(e *auditlog.Event) error {
		if e.Action == action {
			out = append(out, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSSOSignIn(t *testing.T) {
	f := newSSOFixture(t, true, true)
	b := f.browser(t)
	b.tls = true
	authURL := b.start("/#/backups")
	// The flow cookie: sealed, HttpOnly, scoped to the flow, Lax, Secure on TLS.
	c := b.flow
	if !c.HttpOnly || c.Path != "/auth/oidc/" || c.MaxAge != 600 || c.SameSite != http.SameSiteLaxMode || !c.Secure ||
		!strings.HasPrefix(c.Value, secretbox.Prefix) {
		t.Fatalf("flow cookie = %+v", c)
	}
	rec := b.callback(f.idp.Authorize(t, authURL))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#/backups" || b.session == nil || b.flow != nil {
		t.Fatalf("callback: %d %v; session %v, flow %v", rec.Code, rec.Header(), b.session, b.flow)
	}
	if b.session.SameSite != http.SameSiteStrictMode || !b.session.HttpOnly {
		t.Fatalf("session cookie = %+v", b.session)
	}
	me := b.me()
	if me.User == nil || me.User.Username != "jane" || me.User.AuthProvider != auth.ProviderOIDC || me.Role != auth.RoleOperator {
		t.Fatalf("me = %+v", me)
	}
	// The users list shows the provider.
	var users []map[string]any
	decodeData(t, f.admin.do("GET", "/api/v1/users", nil, nil), &users)
	providers := map[string]any{}
	for _, u := range users {
		providers[u["username"].(string)] = u["auth_provider"]
	}
	if providers["jane"] != "oidc" || providers["admin"] != "local" {
		t.Fatalf("providers = %v", providers)
	}
	// Audit: the user, provider=oidc, created and the role.
	ev := f.events(t, oidcCallbackRoute)
	if len(ev) != 1 || ev[0].ActorKind != auditlog.ActorUser || ev[0].ActorName != "jane" || ev[0].Outcome != auditlog.OutcomeOK ||
		ev[0].Targets["provider"] != "oidc" || ev[0].Targets["created"] != "true" || ev[0].Targets["role_to"] != "operator" {
		t.Fatalf("audit = %+v", ev)
	}
	// A second sign-in with another group recomputes the role.
	f.idp.SetClaims(map[string]any{"preferred_username": "jane", "groups": []string{"backup-admins"}})
	b2 := f.browser(t)
	if code := failure(t, func() *httptest.ResponseRecorder { r, _ := b2.login(""); return r }()); code != "" {
		t.Fatalf("second sign-in: %s", code)
	}
	if me = b2.me(); me.Role != auth.RoleAdmin {
		t.Fatalf("recomputed role = %s", me.Role)
	}
	if b.me().User != nil {
		t.Error("the session from before the role change survived")
	}
	ev = f.events(t, oidcCallbackRoute)
	if last := ev[len(ev)-1]; last.Targets["role_from"] != "operator" || last.Targets["role_to"] != "admin" || last.Targets["created"] != "false" {
		t.Fatalf("audit of the role change = %+v", last.Targets)
	}
}

// TestSSOStateAttacks covers forged, missing, expired and replayed state and
// cookies sealed with another key or for another location.
func TestSSOStateAttacks(t *testing.T) {
	f := newSSOFixture(t, true, true)
	ctx := context.Background()

	t.Run("forged state", func(t *testing.T) {
		b := f.browser(t)
		cb := f.idp.Authorize(t, b.start(""))
		q := cb.Query()
		q.Set("state", "forged-"+q.Get("state"))
		cb.RawQuery = q.Encode()
		if code := failure(t, b.callback(cb)); code != auth.OIDCStateMismatch {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("missing cookie", func(t *testing.T) {
		b := f.browser(t)
		cb := f.idp.Authorize(t, b.start(""))
		b.flow = nil
		if code := failure(t, b.callback(cb)); code != auth.OIDCStateMismatch {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("expired cookie", func(t *testing.T) {
		b := f.browser(t)
		cb := f.idp.Authorize(t, b.start(""))
		f.clock.Advance(oidcFlowLifetime + time.Second)
		defer f.clock.Advance(-oidcFlowLifetime - time.Second)
		if code := failure(t, b.callback(cb)); code != auth.OIDCStateMismatch {
			t.Fatalf("code = %q", code)
		}
	})
	for name, seal := range map[string]func(plain string) string{
		"another key": func(plain string) string {
			k, _ := secretbox.GenerateKey()
			other, _ := secretbox.New(k)
			v, _ := other.Seal(oidcFlowBinding, plain)
			return v
		},
		"another binding": func(plain string) string {
			v, _ := f.box.Seal(secretbox.At("cookie", SessionCookieName, "flow"), plain)
			return v
		},
		"plaintext": func(plain string) string { return plain },
		"the right key but no verifier": func(plain string) string {
			var fs flowState
			_ = json.Unmarshal([]byte(plain), &fs)
			fs.Verifier = ""
			raw, _ := json.Marshal(fs)
			v, _ := f.box.Seal(oidcFlowBinding, string(raw))
			return v
		},
	} {
		t.Run("cookie sealed with "+name, func(t *testing.T) {
			b := f.browser(t)
			cb := f.idp.Authorize(t, b.start(""))
			plain, err := f.box.Open(oidcFlowBinding, b.flow.Value)
			if err != nil {
				t.Fatal(err)
			}
			b.flow = &http.Cookie{Name: OIDCFlowCookieName, Value: seal(plain), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
			if code := failure(t, b.callback(cb)); code != auth.OIDCStateMismatch {
				t.Fatalf("code = %q", code)
			}
		})
	}
	t.Run("replayed state", func(t *testing.T) {
		b := f.browser(t)
		cb := f.idp.Authorize(t, b.start(""))
		cookie := b.flow
		if code := failure(t, b.callback(cb)); code != "" {
			t.Fatalf("first callback: %q", code)
		}
		// The attacker replays the same cookie and callback URL.
		replay := f.browser(t)
		replay.flow = cookie
		if code := failure(t, replay.callback(cb)); code != auth.OIDCStateMismatch {
			t.Fatalf("replayed state = %q", code)
		}
	})
	t.Run("replayed code", func(t *testing.T) {
		b := f.browser(t)
		used := f.idp.Authorize(t, b.start(""))
		if code := failure(t, b.callback(used)); code != "" {
			t.Fatal(code)
		}
		// A fresh flow with the old code: the provider refuses it.
		b2 := f.browser(t)
		fresh := f.idp.Authorize(t, b2.start(""))
		q := fresh.Query()
		q.Set("code", used.Query().Get("code"))
		fresh.RawQuery = q.Encode()
		if code := failure(t, b2.callback(fresh)); code != auth.OIDCIdPError {
			t.Fatalf("replayed code = %q", code)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		f.idp.SetAuthorizeError("access_denied")
		defer f.idp.SetAuthorizeError("")
		b := f.browser(t)
		rec, _ := b.login("")
		if code := failure(t, rec); code != auth.OIDCIdPError || strings.Contains(rec.Header().Get("Location"), "refused") {
			t.Fatalf("provider error: %q, %s", code, rec.Header().Get("Location"))
		}
	})
	t.Run("invalid token", func(t *testing.T) {
		f.idp.SetClaimsHook(func(c map[string]any) { c["aud"] = "someone-else" })
		defer f.idp.SetClaimsHook(nil)
		rec, _ := f.browser(t).login("")
		if code := failure(t, rec); code != auth.OIDCTokenInvalid {
			t.Fatalf("code = %q", code)
		}
	})
	// Failures before a verified signature are anonymous and unnamed.
	for _, e := range f.events(t, oidcCallbackRoute) {
		if e.Outcome == auditlog.OutcomeOK {
			continue
		}
		reason := e.Targets["reason"]
		if e.ActorKind != auditlog.ActorAnonymous || e.ActorName != "" || e.ActorUserID != "" || reason == "" || (e.Targets["provider"] != "oidc" && e.Targets[auditlog.TargetCoalescedFrom] == "") {
			t.Errorf("failure entry = %+v", e)
		}
	}
	_ = ctx
}

func TestSSOReturnTo(t *testing.T) {
	for raw, want := range map[string]string{
		"/":                             "/",
		"/#/backups?db=shop":            "/#/backups?db=shop",
		"/settings":                     "/settings",
		"//evil.example.com":            "/",
		"/\\evil.example.com":           "/",
		"https://evil.example.com":      "/",
		"/%2f%2fevil.example.com":       "/",
		"/%2F%5Cevil":                   "/",
		"/x\r\nSet-Cookie: a=b":         "/",
		"/x%0d%0aSet-Cookie:%20a=b":     "/",
		"javascript:alert(1)":           "/",
		"evil.example.com":              "/",
		"/auth/oidc/start":              "/",
		"":                              "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := safeReturnTo(raw); got != want {
			t.Errorf("safeReturnTo(%q) = %q; want %q", raw, got, want)
		}
	}
	f := newSSOFixture(t, true, true)
	for _, evil := range []string{"//evil.example.com", "/\\evil.example.com", "https://evil.example.com", "/%2f%2fevil.example.com", "/x\r\nX: y"} {
		rec, _ := f.browser(t).login(evil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
			t.Errorf("return_to %q: %d → %q; want /", evil, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestSSOLeavesNoTokensBehind proves that tokens, the code, the state, the nonce
// and the verifier appear neither in logs nor in the audit log nor in responses.
func TestSSOLeavesNoTokensBehind(t *testing.T) {
	f := newSSOFixture(t, true, true)
	b := f.browser(t)
	authURL := b.start("")
	plain, err := f.box.Open(oidcFlowBinding, b.flow.Value)
	if err != nil {
		t.Fatal(err)
	}
	var flow flowState
	if err = json.Unmarshal([]byte(plain), &flow); err != nil {
		t.Fatal(err)
	}
	cb := f.idp.Authorize(t, authURL)
	rec := b.callback(cb)
	if failure(t, rec) != "" {
		t.Fatal("sign-in failed")
	}
	// A refused sign-in logs the provider's error, never a code.
	f.idp.SetTokenError("invalid_grant")
	refused, _ := f.browser(t).login("")
	f.idp.SetTokenError("")
	f.log.Flush()
	var audit bytes.Buffer
	_ = f.log.Export(context.Background(), auditlog.Filter{}, func(e *auditlog.Event) error {
		return json.NewEncoder(&audit).Encode(e)
	})
	idToken := f.idp.IDToken(flow.Nonce)
	secrets := map[string]string{
		"code": cb.Query().Get("code"), "state": flow.State, "nonce": flow.Nonce, "verifier": flow.Verifier,
		"access token": "fake-access-token", "refresh token": "fake-refresh-token", "client secret": "client-secret-value",
		"id token header": idToken[:20],
	}
	for name, v := range secrets {
		for where, text := range map[string]string{
			"logs": f.logs.String(), "audit log": audit.String(), "response": rec.Header().Get("Location") + rec.Body.String(),
			"refused response": refused.Header().Get("Location") + refused.Body.String(),
		} {
			if v != "" && strings.Contains(text, v) {
				t.Errorf("the %s holds the %s", where, name)
			}
		}
	}
	// The settings API masks the client secret.
	rec2 := f.admin.do("GET", "/api/v1/settings", nil, nil)
	if strings.Contains(rec2.Body.String(), "client-secret-value") {
		t.Error("GET /api/v1/settings leaks the client secret")
	}
}

func TestSSODisabledAndDesktop(t *testing.T) {
	f := newSSOFixture(t, false, true)
	b := f.browser(t)
	if rec := b.get(oidcStartPath); rec.Code != http.StatusNotFound {
		t.Errorf("start while disabled: %d; want 404", rec.Code)
	}
	if code := failure(t, b.get(oidcCallbackPath+"?code=x&state=y")); code != auth.OIDCDisabled {
		t.Errorf("callback while disabled: %q; want disabled", code)
	}
	var m authMethods
	decodeData(t, b.get(authMethodsPath), &m)
	if m.OIDC.Enabled || !m.OIDC.Available || m.Local != "all" {
		t.Errorf("methods while disabled = %+v", m)
	}

	d := newSSOFixture(t, true, false) // the desktop app: no WithOIDC
	db := d.browser(t)
	for _, p := range []string{oidcStartPath, oidcCallbackPath + "?code=x&state=y"} {
		if rec := db.get(p); rec.Code != http.StatusNotFound {
			t.Errorf("desktop %s: %d; want 404", p, rec.Code)
		}
	}
	decodeData(t, db.get(authMethodsPath), &m)
	if m.OIDC.Enabled || m.OIDC.Available {
		t.Errorf("desktop methods = %+v", m)
	}
	if rec := d.admin.do("POST", "/api/v1/settings/oidc/test", map[string]string{}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("desktop provider test: %d; want 404", rec.Code)
	}
}

func TestSSOAuthMethodsAndProviderTest(t *testing.T) {
	f := newSSOFixture(t, true, true)
	if _, err := f.settings.Update(context.Background(), settings.Patch{OIDC: &settings.OIDCPatch{
		DisplayName: ptrTo("Corp SSO"), LocalLogin: ptrTo("admins_only"),
	}}); err != nil {
		t.Fatal(err)
	}
	var m authMethods
	decodeData(t, f.browser(t).get(authMethodsPath), &m)
	if !m.OIDC.Enabled || m.OIDC.DisplayName != "Corp SSO" || m.Local != "admins_only" {
		t.Fatalf("methods = %+v", m)
	}
	rec := f.admin.do("POST", "/api/v1/settings/oidc/test", map[string]string{}, nil)
	var d oidc.Discovery
	decodeData(t, rec, &d)
	if rec.Code != http.StatusOK || d.Issuer != f.idp.Issuer || d.Keys == 0 || strings.Contains(rec.Body.String(), "client-secret-value") {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if rec = f.admin.do("POST", "/api/v1/settings/oidc/test", map[string]string{"issuer": "http://idp.example.com"}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("test of an http issuer: %d; want 400", rec.Code)
	}
	if rec = f.admin.do("POST", "/api/v1/settings/oidc/test", map[string]string{"issuer": "https://127.0.0.1:1/nothing"}, nil); rec.Code != http.StatusBadGateway {
		t.Errorf("test of an unreachable issuer: %d; want 502", rec.Code)
	}
}

func TestSSOUserRulesOverHTTP(t *testing.T) {
	f := newSSOFixture(t, true, true)
	b := f.browser(t)
	if rec, _ := b.login(""); failure(t, rec) != "" {
		t.Fatal("sign-in failed")
	}
	jane := b.me().User
	// No password for single sign-on users.
	csrf := b.me().CSRFToken
	req := httptest.NewRequest("PUT", "/api/v1/users/"+jane.ID+"/password", strings.NewReader(`{"current_password":"x","new_password":"a long new password"}`))
	req.AddCookie(b.session)
	req.Header.Set(CSRFHeader, csrf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("own password change: %d %s; want 400", rec.Code, rec.Body)
	}
	// Roles of single sign-on users follow the mappings.
	if rec = f.admin.do("PUT", "/api/v1/users/"+jane.ID+"/role", map[string]string{"role": "admin"}, nil); rec.Code != http.StatusConflict {
		t.Errorf("manual role change: %d %s; want 409", rec.Code, rec.Body)
	}
	// The last local admin stays while single sign-on is on.
	f.idp.SetSubject("boss")
	f.idp.SetClaims(map[string]any{"preferred_username": "boss", "groups": []string{"backup-admins"}})
	boss := f.browser(t)
	if rec, _ = boss.login(""); failure(t, rec) != "" {
		t.Fatal("admin sign-in failed")
	}
	bossCSRF := boss.me().CSRFToken
	adminID := f.admin.do("GET", "/api/v1/auth/me", nil, nil)
	var me meResponse
	decodeData(t, adminID, &me)
	del := httptest.NewRequest("DELETE", "/api/v1/users/"+me.User.ID, nil)
	del.AddCookie(boss.session)
	del.Header.Set(CSRFHeader, bossCSRF)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, del)
	if rec.Code != http.StatusConflict {
		t.Errorf("deleting the last local admin: %d %s; want 409", rec.Code, rec.Body)
	}
	// A username collision is a conflict, named in the audit log.
	f.idp.SetSubject("impostor")
	f.idp.SetClaims(map[string]any{"preferred_username": "admin", "groups": []string{"backup-admins"}})
	if rec, _ = f.browser(t).login(""); failure(t, rec) != auth.OIDCAccountConflict {
		t.Errorf("collision: %q", failure(t, rec))
	}
	ev := f.events(t, oidcCallbackRoute)
	if last := ev[len(ev)-1]; last.ActorKind != auditlog.ActorAnonymous || last.ActorName != "admin" || last.Targets["reason"] != auth.OIDCAccountConflict {
		t.Errorf("collision audit = %+v", last)
	}
}

func TestSSOBreakGlassOverHTTP(t *testing.T) {
	f := newSSOFixture(t, true, true)
	if rec := f.admin.do("POST", "/api/v1/users", map[string]string{"username": "carol", "password": testPassword, "role": "operator"}, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create carol: %d", rec.Code)
	}
	if _, err := f.settings.Update(context.Background(), settings.Patch{OIDC: &settings.OIDCPatch{LocalLogin: ptrTo("admins_only")}}); err != nil {
		t.Fatal(err)
	}
	carol := (&authFixture{h: f.h, auth: f.auth}).browser(t)
	if rec := carol.login("carol", "wrong password here"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d; want 401", rec.Code)
	}
	if rec := carol.login("carol", testPassword); rec.Code != http.StatusForbidden {
		t.Errorf("right password: %d; want 403", rec.Code)
	}
	if rec := carol.login("admin", testPassword); rec.Code != http.StatusOK {
		t.Errorf("break-glass admin: %d; want 200", rec.Code)
	}
	var denied *auditlog.Event
	for _, e := range f.events(t, loginRoute) {
		if e.Targets["reason"] == reasonLocalLoginDisabled {
			denied = e
		}
	}
	if denied == nil || denied.Outcome != auditlog.OutcomeDenied || denied.ActorName != "carol" {
		t.Errorf("admins_only refusal audit = %+v", denied)
	}
}

func TestSSOLogoutAtTheProvider(t *testing.T) {
	f := newSSOFixture(t, true, true)
	if _, err := f.settings.Update(context.Background(), settings.Patch{OIDC: &settings.OIDCPatch{RPLogout: ptrTo(true)}}); err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	if rec, _ := b.login(""); failure(t, rec) != "" {
		t.Fatal("sign-in failed")
	}
	csrf := b.me().CSRFToken
	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(b.session)
	req.Header.Set(CSRFHeader, csrf)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var out logoutResponse
	decodeData(t, rec, &out)
	if !out.LoggedOut || !strings.HasPrefix(out.EndSessionURL, f.idp.Issuer+"/logout?") ||
		!strings.Contains(out.EndSessionURL, "post_logout_redirect_uri=https%3A%2F%2Fbackup.example.com%2F") {
		t.Fatalf("logout = %+v", out)
	}
	// Local users get no provider logout.
	rec, out = f.admin.do("POST", "/api/v1/auth/logout", nil, nil), logoutResponse{}
	decodeData(t, rec, &out)
	if out.EndSessionURL != "" {
		t.Errorf("local logout = %+v", out)
	}
}

// withTestOIDC enables the single sign-on routes with a client that is never
// called (single sign-on stays disabled in the settings).
func withTestOIDC(t *testing.T) Option {
	t.Helper()
	key, _ := secretbox.GenerateKey()
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return WithOIDC(oidc.NewClient(&http.Client{Timeout: time.Second}), box)
}

// TestSSORoutesNeedNoCredentials pins the single sign-on browser routes as public
// (outside /api/, answered 404 while single sign-on is off) and the provider test
// as admin-only.
func TestSSORoutesNeedNoCredentials(t *testing.T) {
	f := newScopeFixture(t)
	for _, route := range []string{oidcStartRoute, oidcCallbackRoute} {
		if !slices.Contains(f.srv.patterns, route) {
			t.Fatalf("%s is not registered", route)
		}
		_, path := concrete(route)
		if rec := serve(f.h, http.MethodGet, path, nil, nil); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s without credentials: %d; want the handler's answer", route, rec.Code)
		}
	}
	if routeScopes[oidcTestRoute] != auth.ScopeAdmin {
		t.Errorf("%s needs %q; want admin", oidcTestRoute, routeScopes[oidcTestRoute])
	}
}

func TestSSOStartIsThrottled(t *testing.T) {
	f := newSSOFixture(t, true, true)
	b := f.browser(t)
	var last *httptest.ResponseRecorder
	for i := 0; i <= auth.OIDCAttemptsPerWindow; i++ {
		last = b.get(oidcStartPath)
	}
	if failure(t, last) != auth.OIDCThrottled {
		t.Fatalf("start beyond the budget: %s", last.Header().Get("Location"))
	}
	if len(f.events(t, oidcStartRoute)) != 0 {
		t.Error("starts are not audited")
	}
}
