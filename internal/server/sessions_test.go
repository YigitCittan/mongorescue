package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// sessionsFixture is a scope fixture with the admin signed in and a second user, bob,
// signed in too.
type sessionsFixture struct {
	*scopeFixture
	adminCookie, adminCSRF string
	bobCookie              string
	tokens                 []string
}

func newSessionsFixture(t *testing.T) *sessionsFixture {
	t.Helper()
	f := newScopeFixture(t)
	ctx := context.Background()
	res, err := f.auth.Setup(ctx, "192.0.2.1", f.auth.SetupCode(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := f.auth.AuthenticateSession(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.CreateUser(ctx, admin, "bob", testPassword); err != nil {
		t.Fatal(err)
	}
	bob, err := f.auth.Login(ctx, "192.0.2.1", "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return &sessionsFixture{
		scopeFixture: f,
		adminCookie:  SessionCookieName + "=" + res.Token, adminCSRF: res.CSRFToken,
		bobCookie: SessionCookieName + "=" + bob.Token,
		tokens:    []string{res.Token, bob.Token},
	}
}

// listSessions decodes GET /api/v1/auth/sessions with headers h.
func (f *sessionsFixture) listSessions(t *testing.T, query string, h map[string]string) []auth.SessionInfo {
	t.Helper()
	rec := serve(f.h, "GET", "/api/v1/auth/sessions"+query, nil, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sessions%s = %d %s", query, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", cc)
	}
	for _, token := range f.tokens {
		if body := rec.Body.String(); strings.Contains(body, token) || strings.Contains(body, auth.HashToken(token)) {
			t.Fatalf("session list leaks a token or its hash: %s", body)
		}
	}
	var out struct {
		Data []auth.SessionInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data == nil {
		t.Fatalf("GET sessions%s data is not a list: %s", query, rec.Body)
	}
	return out.Data
}

func TestSessionListIsOwnByDefault(t *testing.T) {
	f := newSessionsFixture(t)
	own := f.listSessions(t, "", map[string]string{"Cookie": f.adminCookie})
	if len(own) != 1 || own[0].Username != "admin" || !own[0].Current {
		t.Fatalf("admin's own sessions = %+v; want the current admin session only", own)
	}
	all := f.listSessions(t, "?all=true", map[string]string{"Cookie": f.adminCookie})
	if len(all) != 2 {
		t.Fatalf("all sessions = %+v; want admin's and bob's", all)
	}
	if rec := serve(f.h, "GET", "/api/v1/auth/sessions?all=maybe", nil, map[string]string{"Cookie": f.adminCookie}); rec.Code != http.StatusBadRequest {
		t.Errorf("?all=maybe = %d; want 400", rec.Code)
	}
}

func TestSessionListAllNeedsAdmin(t *testing.T) {
	f := newSessionsFixture(t)
	for scope, want := range map[auth.Scope]int{auth.ScopeRead: http.StatusForbidden, auth.ScopeOperator: http.StatusForbidden, auth.ScopeAdmin: http.StatusOK} {
		rec := serve(f.h, "GET", "/api/v1/auth/sessions?all=true", nil, map[string]string{"X-API-Key": f.keys[scope]})
		if rec.Code != want {
			t.Errorf("%s key listing all sessions = %d %s; want %d", scope, rec.Code, rec.Body, want)
		}
	}
	// The fixture's keys have no user: their own list is empty.
	if own := f.listSessions(t, "", map[string]string{"X-API-Key": f.keys[auth.ScopeRead]}); len(own) != 0 {
		t.Errorf("userless read key sees %+v; want no sessions", own)
	}
}

func TestSessionListOfAUsersKeyIsThatUsersOnly(t *testing.T) {
	f := newSessionsFixture(t)
	ctx := context.Background()
	bob, err := f.auth.AuthenticateSession(ctx, strings.TrimPrefix(f.bobCookie, SessionCookieName+"="))
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := f.auth.CreateAPIKey(ctx, bob, "bob read", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	own := f.listSessions(t, "", map[string]string{"X-API-Key": key})
	if len(own) != 1 || own[0].UserID != bob.UserID() || own[0].Current {
		t.Fatalf("bob's read key sees %+v; want bob's session only, not current", own)
	}
}

func TestRevokeSession(t *testing.T) {
	f := newSessionsFixture(t)
	all := f.listSessions(t, "?all=true", map[string]string{"Cookie": f.adminCookie})
	var bobID, adminID string
	for _, s := range all {
		if s.Username == "bob" {
			bobID = s.ID
		} else {
			adminID = s.ID
		}
	}
	unsafe := func(token string) map[string]string {
		h := map[string]string{"Cookie": f.adminCookie}
		if token != "" {
			h[CSRFHeader] = token
		}
		return h
	}
	// Without the CSRF token nothing is revoked.
	for _, token := range []string{"", wrongToken(f.adminCSRF)} {
		if rec := serve(f.h, "DELETE", "/api/v1/auth/sessions/"+bobID, nil, unsafe(token)); rec.Code != http.StatusForbidden {
			t.Errorf("revoke with CSRF %q = %d; want 403", token, rec.Code)
		}
	}
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Cookie": f.bobCookie}); rec.Code != http.StatusOK {
		t.Fatalf("bob after refused revokes = %d; want still signed in", rec.Code)
	}
	if rec := serve(f.h, "DELETE", "/api/v1/auth/sessions/ses_000000000000000000000000", nil, unsafe(f.adminCSRF)); rec.Code != http.StatusNotFound {
		t.Errorf("revoking an unknown session = %d; want 404", rec.Code)
	}
	rec := serve(f.h, "DELETE", "/api/v1/auth/sessions/"+bobID, nil, unsafe(f.adminCSRF))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"current":false`) {
		t.Fatalf("revoking bob's session = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("revoking another session cleared the caller's cookie")
	}
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Cookie": f.bobCookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("bob after revoke = %d; want 401", rec.Code)
	}
	// Revoking the current session signs the caller out.
	rec = serve(f.h, "DELETE", "/api/v1/auth/sessions/"+adminID, nil, unsafe(f.adminCSRF))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"current":true`) {
		t.Fatalf("revoking the own session = %d %s", rec.Code, rec.Body)
	}
	if c := rec.Header().Get("Set-Cookie"); !strings.Contains(c, SessionCookieName+"=;") || !strings.Contains(c, "Max-Age=0") {
		t.Errorf("Set-Cookie = %q; want the session cookie cleared", c)
	}
	if rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"Cookie": f.adminCookie}); rec.Code != http.StatusUnauthorized {
		t.Errorf("admin after revoking the own session = %d; want 401", rec.Code)
	}
}
