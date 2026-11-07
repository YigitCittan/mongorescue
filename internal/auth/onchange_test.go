package auth_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// kickCounter counts OnChange calls.
type kickCounter struct{ n atomic.Int32 }

// expectKick runs fn and fails unless it called the OnChange callback.
func (k *kickCounter) expectKick(t *testing.T, what string, fn func()) {
	t.Helper()
	before := k.n.Load()
	fn()
	if k.n.Load() == before {
		t.Errorf("%s did not call the OnChange callback", what)
	}
}

// expectNoKick runs fn and fails if it called the OnChange callback.
func (k *kickCounter) expectNoKick(t *testing.T, what string, fn func()) {
	t.Helper()
	before := k.n.Load()
	fn()
	if k.n.Load() != before {
		t.Errorf("%s called the OnChange callback", what)
	}
}

func TestEveryCommittedSecurityChangeCallsOnChange(t *testing.T) {
	var k kickCounter
	policy := &auth.OIDCPolicy{Enabled: true, LocalLogin: auth.LocalLoginAll, AutoCreateUsers: true}
	f := newFixture(t, auth.WithOIDCPolicy(func() auth.OIDCPolicy { return *policy }))
	f.svc.SetOnChange(func() { k.n.Add(1) })
	ctx := context.Background()

	if _, err := f.svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var setup *auth.LoginResult
	k.expectKick(t, "Setup", func() {
		var err error
		if setup, err = f.svc.Setup(ctx, ip, f.svc.SetupCode(), "admin", adminPassword); err != nil {
			t.Fatal(err)
		}
	})
	admin := f.session(t, setup.Token)

	var bob *auth.User
	k.expectKick(t, "CreateUser", func() {
		var err error
		if bob, err = f.svc.CreateUser(ctx, admin, "bob", adminPassword, auth.RoleViewer); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "SetUserRole", func() {
		if _, err := f.svc.SetUserRole(ctx, admin, bob.ID, auth.RoleOperator); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "SetUserConnections", func() {
		if _, err := f.svc.SetUserConnections(ctx, admin, bob.ID, auth.ConnectionAccess{}); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "ChangePassword of another user", func() {
		if err := f.svc.ChangePassword(ctx, admin, bob.ID, "", "another password 1"); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "ChangePassword of one's own", func() {
		if err := f.svc.ChangePassword(ctx, admin, admin.UserID(), adminPassword, "another password 2"); err != nil {
			t.Fatal(err)
		}
	})
	k.expectNoKick(t, "ChangePassword with a wrong current password", func() {
		if err := f.svc.ChangePassword(ctx, admin, admin.UserID(), "wrong", "another password 3"); err == nil {
			t.Fatal("want an error")
		}
	})

	var key *auth.APIKey
	k.expectKick(t, "CreateAPIKey", func() {
		var err error
		if key, _, err = f.svc.CreateAPIKey(ctx, admin, "ci", auth.ScopeRead); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "GrantAPIKeyAdmin", func() {
		if _, err := f.svc.GrantAPIKeyAdmin(ctx, admin, key.ID); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "DeleteAPIKey", func() {
		if err := f.svc.DeleteAPIKey(ctx, admin, key.ID); err != nil {
			t.Fatal(err)
		}
	})
	k.expectNoKick(t, "DeleteAPIKey of an unknown key", func() {
		if err := f.svc.DeleteAPIKey(ctx, admin, "key_unknown"); err == nil {
			t.Fatal("want an error")
		}
	})
	// Sessions: a plain login changes nothing a takeover could revive.
	var second *auth.LoginResult
	k.expectNoKick(t, "Login", func() {
		var err error
		if second, err = f.svc.Login(ctx, ip, "admin", "another password 2"); err != nil {
			t.Fatal(err)
		}
	})
	sessions, err := f.svc.ListSessions(ctx, admin, false)
	if err != nil {
		t.Fatal(err)
	}
	var other string
	for _, s := range sessions {
		if !s.Current {
			other = s.ID
		}
	}
	k.expectKick(t, "RevokeSession", func() {
		if _, err := f.svc.RevokeSession(ctx, admin, other); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "Logout", func() {
		if err := f.svc.Logout(ctx, second.Token); err != nil {
			t.Fatal(err)
		}
	})

	// Single sign-on: creating a user, applying a role, limiting password sign-in.
	var sso *auth.OIDCLogin
	k.expectKick(t, "LoginOIDC creating a user", func() {
		policy.DefaultRole = auth.RoleViewer
		var err error
		if sso, err = f.svc.LoginOIDC(ctx, identity("1")); err != nil || !sso.Created {
			t.Fatalf("LoginOIDC = %+v, %v", sso, err)
		}
	})
	k.expectNoKick(t, "LoginOIDC of a known user without a change", func() {
		if _, err := f.svc.LoginOIDC(ctx, identity("1")); err != nil {
			t.Fatal(err)
		}
	})
	k.expectKick(t, "ApplyProviderRole", func() {
		if _, err := f.svc.ApplyProviderRole(ctx, admin, sso.User.ID, auth.RoleOperator); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := f.svc.Login(ctx, ip, "bob", "another password 1"); err != nil {
		t.Fatal(err)
	}
	k.expectKick(t, "ApplyOIDCChange ending sessions", func() {
		prev := *policy
		policy.LocalLogin = auth.LocalLoginAdminsOnly
		if err := f.svc.ApplyOIDCChange(ctx, prev, *policy); err != nil {
			t.Fatal(err)
		}
	})

	k.expectKick(t, "ApplyPasswordReset", func() {
		if err := f.svc.ApplyPasswordReset(ctx, admin, bob.ID, "$2a$04$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234"); err != nil {
			t.Fatal(err)
		}
	})

	k.expectKick(t, "DeleteUser", func() {
		if err := f.svc.DeleteUser(ctx, admin, bob.ID); err != nil {
			t.Fatal(err)
		}
	})
}
