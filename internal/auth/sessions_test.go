package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// sessionsFixture has the admin (two sessions) and a second user, bob (one session).
type sessionsFixture struct {
	*fixture
	admin, admin2, bob *auth.Principal
}

func newSessionsFixture(t *testing.T) *sessionsFixture {
	t.Helper()
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	if _, err := f.svc.CreateUser(ctx, f.session(t, res.Token), "bob", adminPassword); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Minute)
	second, err := f.svc.Login(ctx, ip, "admin", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.svc.Login(ctx, ip, "bob", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	return &sessionsFixture{fixture: f, admin: f.session(t, res.Token), admin2: f.session(t, second.Token), bob: f.session(t, bob.Token)}
}

func TestListSessionsIsScopedToTheCaller(t *testing.T) {
	f := newSessionsFixture(t)
	ctx := context.Background()
	own, err := f.svc.ListSessions(ctx, f.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 {
		t.Fatalf("admin's own sessions = %d; want 2", len(own))
	}
	current := 0
	for _, s := range own {
		if s.UserID != f.admin.UserID() || s.Username != "admin" {
			t.Errorf("own list has %+v; want only admin's sessions", s)
		}
		if !strings.HasPrefix(s.ID, "ses_") || len(s.ID) != 28 {
			t.Errorf("session ID %q; want ses_ + 24 hex", s.ID)
		}
		if s.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d sessions marked current; want 1", current)
	}
	all, err := f.svc.ListSessions(ctx, f.admin, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all sessions = %d; want 3", len(all))
	}
	if all[0].Username == "" || all[2].Username == "" {
		t.Errorf("all sessions lack usernames: %+v", all)
	}
}

func TestSessionListNeverCarriesTokensOrHashes(t *testing.T) {
	f := newSessionsFixture(t)
	list, err := f.svc.ListSessions(context.Background(), f.admin, true)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*auth.Principal{f.admin, f.admin2, f.bob} {
		for name, secret := range map[string]string{"token hash": p.SessionHash, "CSRF token": p.CSRFToken} {
			if strings.Contains(string(body), secret) || strings.Contains(string(body), secret[:16]) {
				t.Errorf("session list leaks a %s: %s", name, body)
			}
		}
	}
}

func TestNonAdminPrincipalsSeeAndRevokeOnlyTheirOwnSessions(t *testing.T) {
	f := newSessionsFixture(t)
	ctx := context.Background()
	// A read-scope API key created by bob stands for bob.
	key := &auth.Principal{User: f.bob.User, Method: auth.MethodAPIKey, APIKeyID: "key_x", Scope: auth.ScopeRead}
	if _, err := f.svc.ListSessions(ctx, key, true); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("read key listing all sessions: %v; want ErrForbidden", err)
	}
	own, err := f.svc.ListSessions(ctx, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].UserID != f.bob.UserID() || own[0].Current {
		t.Fatalf("bob's key sees %+v; want bob's one session, not current", own)
	}
	adminSessions, err := f.svc.ListSessions(ctx, f.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RevokeSession(ctx, key, adminSessions[0].ID); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("read key revoking an admin session: %v; want ErrSessionNotFound", err)
	}
	if _, err := f.svc.RevokeSession(ctx, key, own[0].ID); err != nil {
		t.Fatalf("bob's key revoking bob's session: %v", err)
	}
	// Without a user (the imported static key) there are no own sessions.
	static := &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_s", Scope: auth.ScopeOperator}
	if list, err := f.svc.ListSessions(ctx, static, false); err != nil || len(list) != 0 {
		t.Fatalf("static key's own sessions = %v, %v; want none", list, err)
	}
}

func TestRevokeSession(t *testing.T) {
	f := newSessionsFixture(t)
	ctx := context.Background()
	all, err := f.svc.ListSessions(ctx, f.admin, true)
	if err != nil {
		t.Fatal(err)
	}
	var bobID, otherAdminID string
	for _, s := range all {
		switch {
		case s.UserID == f.bob.UserID():
			bobID = s.ID
		case !s.Current:
			otherAdminID = s.ID
		}
	}
	for _, id := range []string{"", "ses_nope", "ses_" + strings.Repeat("0", 24), f.admin.SessionHash} {
		if _, err := f.svc.RevokeSession(ctx, f.admin, id); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Errorf("revoking %q: %v; want ErrSessionNotFound", id, err)
		}
	}
	// An admin may end another user's session.
	current, err := f.svc.RevokeSession(ctx, f.admin, bobID)
	if err != nil || current {
		t.Fatalf("revoking bob's session = %v, %v; want ok, not current", current, err)
	}
	if list, _ := f.svc.ListSessions(ctx, f.bob, false); len(list) != 0 {
		t.Errorf("bob still has %d sessions", len(list))
	}
	if _, err := f.svc.RevokeSession(ctx, f.admin, otherAdminID); err != nil {
		t.Fatal(err)
	}
	// The caller's own session reports current.
	own, err := f.svc.ListSessions(ctx, f.admin, false)
	if err != nil || len(own) != 1 || !own[0].Current {
		t.Fatalf("own sessions = %+v, %v; want only the current one", own, err)
	}
	current, err = f.svc.RevokeSession(ctx, f.admin, own[0].ID)
	if err != nil || !current {
		t.Fatalf("revoking the current session = %v, %v; want current", current, err)
	}
}

func TestSessionListSkipsExpiredSessions(t *testing.T) {
	f := newSessionsFixture(t)
	f.clock.Advance(auth.DefaultIdleTimeout)
	list, err := f.svc.ListSessions(context.Background(), f.admin, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("listed %d idle-expired sessions; want none", len(list))
	}
}

func TestSessionExpiryIsTheEarliestLimit(t *testing.T) {
	f := newSessionsFixture(t)
	list, err := f.svc.ListSessions(context.Background(), f.bob, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("bob's sessions = %v, %v", list, err)
	}
	if want := list[0].LastSeenAt.Add(auth.DefaultIdleTimeout); !list[0].ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v; want the idle limit %v", list[0].ExpiresAt, want)
	}
}
