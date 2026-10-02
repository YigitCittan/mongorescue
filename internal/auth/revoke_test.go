package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// keyPrincipal creates a key of scope as p and returns the key and its principal.
func (f *fixture) keyPrincipal(t *testing.T, p *auth.Principal, name string, scope auth.Scope) (*auth.APIKey, *auth.Principal) {
	t.Helper()
	ctx := context.Background()
	k, plain, err := f.svc.CreateAPIKey(ctx, p, name, scope)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	kp, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	return k, kp
}

func TestNonAdminsSeeOnlyTheirOwnKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	viewer := f.signIn(t, admin, "vera", auth.RoleViewer)
	f.keyPrincipal(t, admin, "admin's", auth.ScopeAdmin)
	own, _ := f.keyPrincipal(t, viewer, "vera's", auth.ScopeRead)
	keys, err := f.svc.ListAPIKeys(ctx, viewer)
	if err != nil || len(keys) != 1 || keys[0].ID != own.ID {
		t.Fatalf("viewer's key list = %+v, %v; want only their own", keys, err)
	}
	if all, _ := f.svc.ListAPIKeys(ctx, admin); len(all) != 2 {
		t.Fatalf("admin's key list = %d keys; want 2", len(all))
	}
}

// TestWhoMayRevokeWhichKey pins DeleteAPIKey: a session may revoke the keys its user
// created, an API key below admin only itself, never its creator's other keys, and
// the admin scope (session or key) any key. Unknown IDs are 404 for everyone.
func TestWhoMayRevokeWhichKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	operator := f.signIn(t, admin, "otto", auth.RoleOperator)

	for _, scope := range []auth.Scope{auth.ScopeRead, auth.ScopeOperator} {
		sibling, _ := f.keyPrincipal(t, operator, "sibling "+string(scope), auth.ScopeRead)
		self, selfP := f.keyPrincipal(t, operator, "self "+string(scope), scope)
		other, _ := f.keyPrincipal(t, admin, "admin's "+string(scope), auth.ScopeRead)
		for _, id := range []string{sibling.ID, other.ID} {
			if err := f.svc.DeleteAPIKey(ctx, selfP, id); !errors.Is(err, auth.ErrForbidden) {
				t.Errorf("%s key revoking %s: %v; want ErrForbidden", scope, id, err)
			}
		}
		if err := f.svc.DeleteAPIKey(ctx, selfP, "key_missing"); !errors.Is(err, auth.ErrAPIKeyNotFound) {
			t.Errorf("%s key revoking an unknown key: %v; want ErrAPIKeyNotFound", scope, err)
		}
		// A key revokes itself.
		if err := f.svc.DeleteAPIKey(ctx, selfP, self.ID); err != nil {
			t.Errorf("%s key revoking itself: %v", scope, err)
		}
		// The owner's session revokes the owner's keys, not another user's.
		if err := f.svc.DeleteAPIKey(ctx, operator, other.ID); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("operator session revoking the admin's key: %v; want ErrForbidden", err)
		}
		if err := f.svc.DeleteAPIKey(ctx, operator, sibling.ID); err != nil {
			t.Errorf("operator session revoking their own key: %v", err)
		}
	}
	if err := f.svc.DeleteAPIKey(ctx, operator, "key_missing"); !errors.Is(err, auth.ErrAPIKeyNotFound) {
		t.Fatalf("session revoking an unknown key: %v; want ErrAPIKeyNotFound", err)
	}

	// The admin scope revokes any key, by session or by key.
	victim, _ := f.keyPrincipal(t, operator, "victim", auth.ScopeRead)
	_, adminKey := f.keyPrincipal(t, admin, "root", auth.ScopeAdmin)
	if err := f.svc.DeleteAPIKey(ctx, adminKey, victim.ID); err != nil {
		t.Fatalf("admin key revoking another user's key: %v", err)
	}
	victim, _ = f.keyPrincipal(t, operator, "victim 2", auth.ScopeRead)
	if err := f.svc.DeleteAPIKey(ctx, admin, victim.ID); err != nil {
		t.Fatalf("admin session revoking another user's key: %v", err)
	}
}

// TestWhoMayRevokeWhichSession pins RevokeSession: a session may end its own user's
// sessions, an API key below admin none at all, and the admin scope any.
func TestWhoMayRevokeWhichSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	viewer := f.signIn(t, admin, "vera", auth.RoleViewer)
	second := f.signIn(t, admin, "otto", auth.RoleViewer)
	sessions := func() map[string]string {
		list, err := f.svc.ListSessions(ctx, admin, true)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, s := range list {
			out[s.ID] = s.UserID
		}
		return out
	}
	idOf := func(userID string) string {
		for id, uid := range sessions() {
			if uid == userID {
				return id
			}
		}
		t.Fatalf("no session of %s", userID)
		return ""
	}

	// Keys below admin end no session, not even their creator's.
	_, readKey := f.keyPrincipal(t, viewer, "vera read", auth.ScopeRead)
	for _, id := range []string{idOf(viewer.User.ID), idOf(second.User.ID)} {
		if _, err := f.svc.RevokeSession(ctx, readKey, id); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("read key revoking %s: %v; want ErrForbidden", id, err)
		}
	}
	_, opKeyOfAdmin := f.keyPrincipal(t, admin, "admin operator", auth.ScopeOperator)
	if _, err := f.svc.RevokeSession(ctx, opKeyOfAdmin, idOf(admin.User.ID)); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("operator key revoking its creator's session: %v; want ErrForbidden", err)
	}

	// A viewer session ends its own user's sessions, not another user's.
	if _, err := f.svc.RevokeSession(ctx, viewer, idOf(second.User.ID)); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("viewer session revoking another user's session: %v; want ErrForbidden", err)
	}
	current, err := f.svc.RevokeSession(ctx, viewer, idOf(viewer.User.ID))
	if err != nil || !current {
		t.Errorf("viewer session revoking itself = %v, %v; want ok, current", current, err)
	}

	// An admin key ends any session.
	_, adminKey := f.keyPrincipal(t, admin, "root", auth.ScopeAdmin)
	if _, err = f.svc.RevokeSession(ctx, adminKey, idOf(second.User.ID)); err != nil {
		t.Errorf("admin key revoking another user's session: %v", err)
	}
}

func TestDeletingAUserNeedsAnActorWhoIsStillAdmin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	alice := f.session(t, f.setup(t).Token)
	bob := f.signIn(t, alice, "bob", auth.RoleAdmin)
	carol, err := f.svc.CreateUser(ctx, alice, "carol", adminPassword, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SetUserRole(ctx, bob, alice.User.ID, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	// alice's principal still says admin, but she no longer is: the store refuses.
	if err = f.svc.DeleteUser(ctx, alice, carol.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("a demoted admin deleting a user: %v; want ErrForbidden", err)
	}
	if err = f.svc.DeleteUser(ctx, bob, carol.ID); err != nil {
		t.Fatalf("an admin deleting a user: %v", err)
	}
}
