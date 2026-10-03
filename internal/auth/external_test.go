package auth_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

const issuer = "https://idp.example.com"

// ssoFixture is a fixture whose single sign-on policy the test changes freely.
type ssoFixture struct {
	*fixture
	policy *auth.OIDCPolicy
	admin  *auth.LoginResult
}

func newSSOFixture(t *testing.T, opts ...auth.Option) *ssoFixture {
	t.Helper()
	policy := &auth.OIDCPolicy{
		Enabled: true, LocalLogin: auth.LocalLoginAll, AutoCreateUsers: true,
		RoleMappings: []auth.RoleMapping{{Group: "ops", Role: auth.RoleOperator}, {Group: "admins", Role: auth.RoleAdmin}},
	}
	opts = append(opts, auth.WithOIDCPolicy(func() auth.OIDCPolicy { return *policy }))
	f := &ssoFixture{fixture: newFixture(t, opts...), policy: policy}
	f.admin = f.setup(t)
	return f
}

func identity(sub string, groups ...string) *auth.ExternalIdentity {
	return &auth.ExternalIdentity{Issuer: issuer, Subject: sub, Username: "user-" + sub, Email: sub + "@corp.com", EmailVerified: true, Groups: groups}
}

func TestLoginOIDCCreatesFindsAndRecomputes(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	first, err := f.svc.LoginOIDC(ctx, identity("1", "ops"))
	if err != nil || !first.Created || first.RoleTo != auth.RoleOperator || first.User.AuthProvider != auth.ProviderOIDC ||
		first.Token == "" || first.CSRFToken == "" {
		t.Fatalf("first sign-in = %+v, %v", first, err)
	}
	p := f.session(t, first.Token)
	if p.Role != auth.RoleOperator || p.User.Username != "user-1" {
		t.Fatalf("session = %+v", p)
	}
	// Highest role wins, and the change ends the earlier session.
	second, err := f.svc.LoginOIDC(ctx, identity("1", "ops", "admins"))
	if err != nil || second.Created || second.RoleFrom != auth.RoleOperator || second.RoleTo != auth.RoleAdmin || second.User.ID != first.User.ID {
		t.Fatalf("second sign-in = %+v, %v", second, err)
	}
	if _, err = f.svc.AuthenticateSession(ctx, first.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("the session from before the role change = %v; want ended", err)
	}
	// No group, no default role: denied.
	if _, err = f.svc.LoginOIDC(ctx, identity("1")); !errors.Is(err, auth.ErrNoRole) || auth.OIDCErrorCode(err) != auth.OIDCNoRole {
		t.Errorf("no group = %v; want ErrNoRole", err)
	}
	// The default role applies when no mapping matches; it never grants admin.
	f.policy.DefaultRole = auth.RoleViewer
	if res, loginErr := f.svc.LoginOIDC(ctx, identity("2", "unmapped")); loginErr != nil || res.RoleTo != auth.RoleViewer {
		t.Errorf("default role = %+v, %v", res, loginErr)
	}
	f.policy.DefaultRole = auth.RoleAdmin
	if _, err = f.svc.LoginOIDC(ctx, identity("3")); !errors.Is(err, auth.ErrNoRole) {
		t.Errorf("admin as the default role = %v; want ErrNoRole", err)
	}
}

// TestDefaultRoleAppliesOnlyToNewUsersWithoutMappings pins the two role policies:
// with group mappings the role follows the groups at every sign-in; without any,
// the default role is only the role of a new user and a role set by hand stays.
func TestDefaultRoleAppliesOnlyToNewUsersWithoutMappings(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.admin.Token)

	// Without mappings (Google style): the default role at creation only.
	f.policy.RoleMappings = nil
	f.policy.DefaultRole = auth.RoleViewer
	first, err := f.svc.LoginOIDC(ctx, identity("g1"))
	if err != nil || !first.Created || first.RoleTo != auth.RoleViewer {
		t.Fatalf("first sign-in = %+v, %v; want a new viewer", first, err)
	}
	// The role select stays usable: an administrator promotes the user by hand.
	if _, err = f.svc.SetUserRole(ctx, admin, first.User.ID, auth.RoleOperator); err != nil {
		t.Fatalf("manual role change without mappings: %v", err)
	}
	again, err := f.svc.LoginOIDC(ctx, identity("g1"))
	if err != nil || again.Created || again.RoleFrom != auth.RoleOperator || again.RoleTo != auth.RoleOperator {
		t.Fatalf("second sign-in = %+v, %v; want the hand-set operator role kept", again, err)
	}
	if session := f.session(t, again.Token); session.Role != auth.RoleOperator {
		t.Fatalf("session role = %s; want operator", session.Role)
	}
	// No role change, so signing in again ends no session.
	third, err := f.svc.LoginOIDC(ctx, identity("g1"))
	if err != nil {
		t.Fatal(err)
	}
	f.session(t, again.Token)
	f.session(t, third.Token)
	// A deny default still lets existing users in with their stored role, and
	// refuses new ones.
	f.policy.DefaultRole = ""
	if res, loginErr := f.svc.LoginOIDC(ctx, identity("g1")); loginErr != nil || res.RoleTo != auth.RoleOperator {
		t.Fatalf("existing user with a deny default = %+v, %v", res, loginErr)
	}
	if _, err = f.svc.LoginOIDC(ctx, identity("g2")); !errors.Is(err, auth.ErrNoRole) || auth.OIDCErrorCode(err) != auth.OIDCNoRole {
		t.Fatalf("new user with a deny default = %v; want ErrNoRole", err)
	}

	// With mappings the role is recomputed at every sign-in and replaces the
	// hand-set one (and manual changes are refused).
	f.policy.RoleMappings = []auth.RoleMapping{{Group: "ops", Role: auth.RoleOperator}}
	f.policy.DefaultRole = auth.RoleViewer
	res, err := f.svc.LoginOIDC(ctx, identity("g1"))
	if err != nil || res.RoleFrom != auth.RoleOperator || res.RoleTo != auth.RoleViewer {
		t.Fatalf("sign-in with mappings = %+v, %v; want the default viewer role applied", res, err)
	}
	if _, err = f.svc.SetUserRole(ctx, admin, first.User.ID, auth.RoleOperator); !errors.Is(err, auth.ErrRoleManagedByProvider) {
		t.Fatalf("manual role change with mappings = %v; want ErrRoleManagedByProvider", err)
	}
	if res, err = f.svc.LoginOIDC(ctx, identity("g1", "ops")); err != nil || res.RoleTo != auth.RoleOperator {
		t.Fatalf("sign-in in a mapped group = %+v, %v", res, err)
	}
}

func TestLoginOIDCRefusals(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	// A username collision with the local admin never links the accounts.
	id := identity("x", "admins")
	id.Username = "ADMIN"
	if _, err := f.svc.LoginOIDC(ctx, id); !errors.Is(err, auth.ErrAccountConflict) || auth.OIDCErrorCode(err) != auth.OIDCAccountConflict {
		t.Errorf("collision = %v; want ErrAccountConflict", err)
	}
	// Without auto-creation unknown identities are refused.
	f.policy.AutoCreateUsers = false
	if _, err := f.svc.LoginOIDC(ctx, identity("new", "ops")); !errors.Is(err, auth.ErrUnknownExternalUser) || auth.OIDCErrorCode(err) != auth.OIDCNoRole {
		t.Errorf("auto-create off = %v; want ErrUnknownExternalUser", err)
	}
	f.policy.AutoCreateUsers = true
	// Incomplete identities.
	for _, bad := range []*auth.ExternalIdentity{nil, {Subject: "s", Username: "u", Groups: []string{"ops"}},
		{Issuer: issuer, Username: "u", Groups: []string{"ops"}}, {Issuer: issuer, Subject: "s", Username: "bad name!", Groups: []string{"ops"}}} {
		if _, err := f.svc.LoginOIDC(ctx, bad); !errors.Is(err, auth.ErrInvalidIdentity) || auth.OIDCErrorCode(err) != auth.OIDCTokenInvalid {
			t.Errorf("identity %+v = %v; want ErrInvalidIdentity", bad, err)
		}
	}
	// Disabled.
	f.policy.Enabled = false
	if _, err := f.svc.LoginOIDC(ctx, identity("1", "ops")); !errors.Is(err, auth.ErrOIDCDisabled) || auth.OIDCErrorCode(err) != auth.OIDCDisabled {
		t.Errorf("disabled = %v; want ErrOIDCDisabled", err)
	}
}

func TestEmailDomainFilter(t *testing.T) {
	f := newSSOFixture(t)
	f.policy.AllowedEmailDomains = []string{"corp.com"}
	ctx := context.Background()
	cases := []struct {
		email    string
		verified bool
		allowed  bool
	}{
		{"jane@corp.com", true, true},
		{"X@CORP.COM", true, true}, // domains are case-insensitive
		{"x@corp.com.evil.com", true, false},
		{"x@evilcorp.com", true, false},
		{"x@sub.corp.com", true, false},
		{"jane@corp.com", false, false}, // email_verified false, the string "true" or missing
		{"", true, false},
		{"corp.com", true, false},
		{"x@", true, false},
		{"@corp.com", true, false},
	}
	for i, c := range cases {
		id := identity(string(rune('a'+i)), "ops")
		id.Email, id.EmailVerified = c.email, c.verified
		_, err := f.svc.LoginOIDC(ctx, id)
		switch {
		case c.allowed && err != nil:
			t.Errorf("%q (verified %v) = %v; want allowed", c.email, c.verified, err)
		case !c.allowed && (!errors.Is(err, auth.ErrDomainNotAllowed) || auth.OIDCErrorCode(err) != auth.OIDCDomainNotAllowed):
			t.Errorf("%q (verified %v) = %v; want ErrDomainNotAllowed", c.email, c.verified, err)
		}
	}
	// Without a filter unverified emails are fine.
	f.policy.AllowedEmailDomains = nil
	id := identity("free", "ops")
	id.EmailVerified = false
	if _, err := f.svc.LoginOIDC(ctx, id); err != nil {
		t.Errorf("no filter: %v", err)
	}
}

// TestPasswordFormForSSOUsers checks that an OIDC user can never sign in with a
// password and takes exactly one comparison against the dummy hash, the timing of an
// unknown user, and that password operations refuse them.
func TestPasswordFormForSSOUsers(t *testing.T) {
	var comparisons atomic.Int32
	counting := func(_, _ []byte) error {
		comparisons.Add(1)
		return errors.New("mismatch")
	}
	f := newSSOFixture(t, auth.WithPasswordComparer(counting))
	ctx := context.Background()
	sso, err := f.svc.LoginOIDC(ctx, identity("1", "admins"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{"", "anything at all", adminPassword} {
		comparisons.Store(0)
		if _, err = f.svc.Login(ctx, ip, "user-1", pw); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("password %q for an oidc user = %v; want ErrInvalidCredentials", pw, err)
		}
		if n := comparisons.Load(); n != 1 {
			t.Errorf("password %q: %d comparisons; want exactly 1 (the dummy hash)", pw, n)
		}
	}
	p := f.session(t, sso.Token)
	if err = f.svc.ChangePassword(ctx, p, p.User.ID, "x", "a brand new password"); !errors.Is(err, auth.ErrNoPassword) {
		t.Errorf("own password change = %v; want ErrNoPassword", err)
	}
	admin := f.session(t, f.admin.Token)
	if err = f.svc.ChangePassword(ctx, admin, p.User.ID, "", "a brand new password"); !errors.Is(err, auth.ErrNoPassword) {
		t.Errorf("admin password reset of an oidc user = %v; want ErrNoPassword", err)
	}
	if err = f.svc.ConfirmPassword(ctx, p, "anything"); !errors.Is(err, auth.ErrNoPassword) {
		t.Errorf("ConfirmPassword = %v; want ErrNoPassword (no recovery kit for oidc admins)", err)
	}
}

func TestBreakGlassAdminsOnly(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.admin.Token)
	if _, err := f.svc.CreateUser(ctx, admin, "carol", adminPassword, auth.RoleOperator); err != nil {
		t.Fatal(err)
	}
	carol, err := f.svc.Login(ctx, ip, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	prev := *f.policy
	f.policy.LocalLogin = auth.LocalLoginAdminsOnly
	if err = f.svc.ApplyOIDCChange(ctx, prev, *f.policy); err != nil {
		t.Fatal(err)
	}
	// Switching admins_only on ended carol's session, not the admin's.
	if _, err = f.svc.AuthenticateSession(ctx, carol.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("local non-admin session = %v; want ended", err)
	}
	f.session(t, f.admin.Token)
	// A wrong password stays a generic failure; the right one is refused only now.
	if _, err = f.svc.Login(ctx, ip, "carol", "wrong password!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password = %v; want ErrInvalidCredentials", err)
	}
	if _, err = f.svc.Login(ctx, ip, "carol", adminPassword); !errors.Is(err, auth.ErrLocalLoginDisabled) {
		t.Errorf("right password = %v; want ErrLocalLoginDisabled", err)
	}
	if _, err = f.svc.Login(ctx, ip, "admin", adminPassword); err != nil {
		t.Errorf("break-glass admin: %v", err)
	}
	// admins_only means nothing while single sign-on is off.
	f.policy.Enabled = false
	if _, err = f.svc.Login(ctx, ip, "carol", adminPassword); err != nil {
		t.Errorf("sso off: %v", err)
	}
}

func TestLastLocalAdminAndManagedRoles(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	sso, err := f.svc.LoginOIDC(ctx, identity("1", "admins"))
	if err != nil {
		t.Fatal(err)
	}
	ssoAdmin := f.session(t, sso.Token)
	localAdmin := f.session(t, f.admin.Token)
	// Another admin exists, but the local admin is the way in when the IdP is down.
	if _, err = f.svc.SetUserRole(ctx, ssoAdmin, localAdmin.User.ID, auth.RoleViewer); !errors.Is(err, auth.ErrLastLocalAdmin) {
		t.Errorf("demote the last local admin = %v; want ErrLastLocalAdmin", err)
	}
	if err = f.svc.DeleteUser(ctx, ssoAdmin, localAdmin.User.ID); !errors.Is(err, auth.ErrLastLocalAdmin) {
		t.Errorf("delete the last local admin = %v; want ErrLastLocalAdmin", err)
	}
	// Roles of OIDC users are managed by the provider while mappings exist.
	if _, err = f.svc.SetUserRole(ctx, localAdmin, sso.User.ID, auth.RoleViewer); !errors.Is(err, auth.ErrRoleManagedByProvider) {
		t.Errorf("manual role change = %v; want ErrRoleManagedByProvider", err)
	}
	f.policy.RoleMappings = nil
	f.policy.DefaultRole = auth.RoleViewer
	if change, setErr := f.svc.SetUserRole(ctx, localAdmin, sso.User.ID, auth.RoleOperator); setErr != nil || change.User.Role != auth.RoleOperator {
		t.Errorf("manual role change without mappings = %+v, %v", change, setErr)
	}
	// Turning single sign-on on needs a local admin.
	if err = f.svc.CheckOIDCChange(ctx, auth.OIDCPolicy{}, auth.OIDCPolicy{Enabled: true}); err != nil {
		t.Errorf("enable with a local admin: %v", err)
	}
	if n, countErr := f.svc.CountLocalAdmins(ctx); countErr != nil || n != 1 {
		t.Errorf("CountLocalAdmins = %d, %v", n, countErr)
	}
}

func TestCheckOIDCChangeWithoutLocalAdmin(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	if _, err := f.svc.LoginOIDC(ctx, identity("1", "admins")); err != nil {
		t.Fatal(err)
	}
	f.policy.Enabled = false // the rule only holds while sso is on
	local := f.session(t, f.admin.Token)
	if err := f.svc.DeleteUser(ctx, f.sessionOf(t, "1"), local.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, next := range []auth.OIDCPolicy{{Enabled: true}, {LocalLogin: auth.LocalLoginAdminsOnly}} {
		if err := f.svc.CheckOIDCChange(ctx, auth.OIDCPolicy{}, next); !errors.Is(err, auth.ErrLastLocalAdmin) {
			t.Errorf("CheckOIDCChange(%+v) = %v; want ErrLastLocalAdmin", next, err)
		}
	}
	// Changes that neither enable nor limit pass.
	if err := f.svc.CheckOIDCChange(ctx, auth.OIDCPolicy{Enabled: true}, auth.OIDCPolicy{Enabled: true}); err != nil {
		t.Errorf("unrelated change: %v", err)
	}
}

// sessionOf signs the OIDC user with subject sub in and returns the principal.
func (f *ssoFixture) sessionOf(t *testing.T, sub string) *auth.Principal {
	t.Helper()
	policy := *f.policy
	f.policy.Enabled = true
	defer func() { *f.policy = policy }()
	res, err := f.svc.LoginOIDC(context.Background(), identity(sub, "admins"))
	if err != nil {
		t.Fatal(err)
	}
	return f.session(t, res.Token)
}

func TestOIDCAttemptThrottle(t *testing.T) {
	f := newSSOFixture(t)
	for i := 0; i < auth.OIDCAttemptsPerWindow; i++ {
		if err := f.svc.AllowOIDCAttempt("start", ip); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	err := f.svc.AllowOIDCAttempt("start", ip)
	var throttled *auth.ThrottledError
	if !errors.As(err, &throttled) || auth.OIDCErrorCode(err) != auth.OIDCThrottled {
		t.Fatalf("over budget = %v; want a *ThrottledError", err)
	}
	if err = f.svc.AllowOIDCAttempt("start", "198.51.100.1"); err != nil {
		t.Errorf("another address: %v", err)
	}
	f.clock.Advance(auth.OIDCAttemptWindow)
	if err = f.svc.AllowOIDCAttempt("start", ip); err != nil {
		t.Errorf("after the window: %v", err)
	}
}
