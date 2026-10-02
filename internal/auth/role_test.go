package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// signIn creates a user with role (as admin) and returns their session principal.
func (f *fixture) signIn(t *testing.T, admin *auth.Principal, name string, role auth.Role) *auth.Principal {
	t.Helper()
	ctx := context.Background()
	if _, err := f.svc.CreateUser(ctx, admin, name, adminPassword, role); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	res, err := f.svc.Login(ctx, ip, name, adminPassword)
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	return f.session(t, res.Token)
}

func TestParseRole(t *testing.T) {
	for _, r := range auth.Roles() {
		if got, err := auth.ParseRole(string(r)); err != nil || got != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, err)
		}
	}
	for _, bad := range []string{"", "root", "Admin"} {
		if _, err := auth.ParseRole(bad); !errors.Is(err, auth.ErrInvalidRole) {
			t.Errorf("ParseRole(%q) = %v; want ErrInvalidRole", bad, err)
		}
	}
	for r, want := range map[auth.Role]auth.Scope{
		auth.RoleViewer: auth.ScopeRead, auth.RoleOperator: auth.ScopeOperator, auth.RoleAdmin: auth.ScopeAdmin, "": auth.ScopeRead, "root": auth.ScopeRead,
	} {
		if got := r.Scope(); got != want {
			t.Errorf("Role(%q).Scope() = %q; want %q", r, got, want)
		}
	}
}

func TestSetupCreatesAnAdminAndNewUsersDefaultToViewer(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	if res.User.Role != auth.RoleAdmin {
		t.Fatalf("first user role = %q; want admin", res.User.Role)
	}
	admin := f.session(t, res.Token)
	if admin.Role != auth.RoleAdmin || admin.Scope != auth.ScopeAdmin {
		t.Fatalf("admin session = %+v", admin)
	}
	u, err := f.svc.CreateUser(ctx, admin, "dora", adminPassword, "")
	if err != nil || u.Role != auth.RoleViewer {
		t.Fatalf("user created without a role = %+v, %v; want viewer", u, err)
	}
	if _, err = f.svc.CreateUser(ctx, admin, "eve", adminPassword, "root"); !errors.Is(err, auth.ErrInvalidRole) {
		t.Fatalf("unknown role: %v", err)
	}
	// The store never lets the column default (admin) apply to a new user.
	if err = f.store.CreateUser(ctx, &auth.User{ID: "usr_norole", Username: "norole", PasswordHash: "x"}); !errors.Is(err, auth.ErrInvalidRole) {
		t.Fatalf("store insert without a role: %v; want ErrInvalidRole", err)
	}
}

func TestSessionScopeFollowsTheRole(t *testing.T) {
	f := newFixture(t)
	admin := f.session(t, f.setup(t).Token)
	for _, tc := range []struct {
		name string
		role auth.Role
		want auth.Scope
	}{{"vera", auth.RoleViewer, auth.ScopeRead}, {"otto", auth.RoleOperator, auth.ScopeOperator}, {"ada", auth.RoleAdmin, auth.ScopeAdmin}} {
		p := f.signIn(t, admin, tc.name, tc.role)
		if p.Role != tc.role || p.Scope != tc.want || p.ScopeSource() != auth.SourceRole {
			t.Errorf("%s session = role %q scope %q source %q; want %q %q", tc.name, p.Role, p.Scope, p.ScopeSource(), tc.role, tc.want)
		}
	}
}

func TestAPIKeysAreCappedByTheirCreatorsCurrentRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	bob := f.signIn(t, admin, "bob", auth.RoleAdmin)
	k, plain, err := f.svc.CreateAPIKey(ctx, bob, "bob's automation", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if p, authErr := f.svc.AuthenticateAPIKey(ctx, plain); authErr != nil || p.Scope != auth.ScopeAdmin || p.KeyScope != auth.ScopeAdmin {
		t.Fatalf("key of an admin = %+v, %v", p, authErr)
	}

	if _, err = f.svc.SetUserRole(ctx, admin, bob.User.ID, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil || p.Scope != auth.ScopeRead || p.KeyScope != auth.ScopeAdmin || p.Role != auth.RoleViewer ||
		p.ScopeSource() != auth.SourceKeyCappedByRole {
		t.Fatalf("key of a demoted creator = %+v (source %q), %v; want read, capped", p, p.ScopeSource(), err)
	}
	var se *auth.ScopeError
	if err = p.Require(auth.ScopeOperator); !errors.As(err, &se) || se.Source != auth.SourceKeyCappedByRole || se.KeyScope != auth.ScopeAdmin {
		t.Fatalf("capped key requiring operator: %v", err)
	}
	keys, err := f.svc.ListAPIKeys(ctx, admin)
	if err != nil || len(keys) != 1 || keys[0].ID != k.ID || keys[0].Scope != auth.ScopeAdmin || keys[0].EffectiveScope != auth.ScopeRead {
		t.Fatalf("key list = %+v, %v; want admin capped to read", keys, err)
	}

	// Promoting the creator lifts the cap on the next request.
	if _, err = f.svc.SetUserRole(ctx, admin, bob.User.ID, auth.RoleOperator); err != nil {
		t.Fatal(err)
	}
	if p, err = f.svc.AuthenticateAPIKey(ctx, plain); err != nil || p.Scope != auth.ScopeOperator {
		t.Fatalf("key of an operator creator = %+v, %v", p, err)
	}
}

func TestAPIKeyScopeCannotExceedTheCreator(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	viewer := f.signIn(t, admin, "vera", auth.RoleViewer)
	operator := f.signIn(t, admin, "otto", auth.RoleOperator)

	for _, tc := range []struct {
		p     *auth.Principal
		scope auth.Scope
		ok    bool
	}{
		{viewer, auth.ScopeRead, true}, {viewer, auth.ScopeOperator, false}, {viewer, auth.ScopeAdmin, false},
		{operator, auth.ScopeOperator, true}, {operator, auth.ScopeAdmin, false},
		{admin, auth.ScopeAdmin, true},
	} {
		_, _, err := f.svc.CreateAPIKey(ctx, tc.p, "k", tc.scope)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, auth.ErrScopeExceedsRole)) {
			t.Errorf("%s creating a %s key: %v; want ok=%v", tc.p.Role, tc.scope, err, tc.ok)
		}
	}

	// API keys create keys only with the admin scope.
	_, opKey, err := f.svc.CreateAPIKey(ctx, operator, "otto's key", auth.ScopeOperator)
	if err != nil {
		t.Fatal(err)
	}
	opP, err := f.svc.AuthenticateAPIKey(ctx, opKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.CreateAPIKey(ctx, opP, "child", auth.ScopeRead); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator key creating a key: %v; want ErrForbidden", err)
	}
	_, adminKey, err := f.svc.CreateAPIKey(ctx, admin, "root", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	adminP, err := f.svc.AuthenticateAPIKey(ctx, adminKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.CreateAPIKey(ctx, adminP, "child", auth.ScopeAdmin); err != nil {
		t.Fatalf("admin key creating a key: %v", err)
	}
}

func TestUserAdministrationNeedsAdminInTheService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	viewer := f.signIn(t, admin, "vera", auth.RoleViewer)
	operator := f.signIn(t, admin, "otto", auth.RoleOperator)
	for _, p := range []*auth.Principal{viewer, operator} {
		if _, err := f.svc.ListUsers(ctx, p); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s ListUsers: %v", p.Role, err)
		}
		if _, err := f.svc.CreateUser(ctx, p, "mallory", adminPassword, auth.RoleAdmin); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s CreateUser: %v", p.Role, err)
		}
		if err := f.svc.DeleteUser(ctx, p, admin.User.ID); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s DeleteUser: %v", p.Role, err)
		}
		if _, err := f.svc.SetUserRole(ctx, p, p.User.ID, auth.RoleAdmin); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s promoting themselves: %v", p.Role, err)
		}
		if err := f.svc.ChangePassword(ctx, p, admin.User.ID, "", "a brand new password"); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s changing the admin's password: %v", p.Role, err)
		}
		// Every role may change its own password with the current one.
		if err := f.svc.ChangePassword(ctx, p, p.User.ID, adminPassword, adminPassword+"!"); err != nil {
			t.Errorf("%s changing their own password: %v", p.Role, err)
		}
		// Every role may list user names.
		if names, err := f.svc.ListUserNames(ctx, p); err != nil || len(names) != 3 {
			t.Errorf("%s ListUserNames = %+v, %v", p.Role, names, err)
		}
	}

	// Changing one's own password needs a session, not an API key.
	_, plain, err := f.svc.CreateAPIKey(ctx, viewer, "vera's", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.svc.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.ChangePassword(ctx, key, viewer.User.ID, adminPassword+"!", "another new password"); !errors.Is(err, auth.ErrSessionRequired) {
		t.Fatalf("own password with an API key: %v; want ErrSessionRequired", err)
	}
}

func TestLastAdminIsProtected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	carol := f.signIn(t, admin, "carol", auth.RoleOperator)

	if _, err := f.svc.SetUserRole(ctx, admin, admin.User.ID, auth.RoleViewer); !errors.Is(err, auth.ErrChangeOwnRole) {
		t.Fatalf("changing your own role: %v", err)
	}
	if _, err := f.svc.SetUserRole(ctx, admin, carol.User.ID, "root"); !errors.Is(err, auth.ErrInvalidRole) {
		t.Fatalf("unknown role: %v", err)
	}
	if _, err := f.svc.SetUserRole(ctx, admin, "usr_missing", auth.RoleViewer); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// The system (no user) may change roles but cannot demote or delete the last admin.
	system := auth.SystemPrincipal()
	if _, err := f.svc.SetUserRole(ctx, system, admin.User.ID, auth.RoleOperator); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("demoting the last admin: %v; want ErrLastAdmin", err)
	}
	if err := f.svc.DeleteUser(ctx, system, admin.User.ID); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("deleting the last admin: %v; want ErrLastAdmin", err)
	}

	// A role change ends the user's sessions.
	change, err := f.svc.SetUserRole(ctx, admin, carol.User.ID, auth.RoleAdmin)
	if err != nil || change.From != auth.RoleOperator || change.User.Role != auth.RoleAdmin {
		t.Fatalf("promoting carol = %+v, %v", change, err)
	}
	if _, err = f.svc.AuthenticateSession(ctx, carolToken(t, f)); err != nil {
		t.Fatalf("a fresh session after the change: %v", err)
	}
	if list, _ := f.svc.ListSessions(ctx, admin, true); countUser(list, carol.User.ID) != 1 {
		t.Fatalf("carol's sessions after the change = %d; want only the fresh one", countUser(list, carol.User.ID))
	}
	// With two admins, either may be demoted, and then the other is the last.
	if _, err = f.svc.SetUserRole(ctx, system, admin.User.ID, auth.RoleViewer); err != nil {
		t.Fatalf("demoting one of two admins: %v", err)
	}
	if err = f.svc.DeleteUser(ctx, system, carol.User.ID); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("deleting the remaining admin: %v; want ErrLastAdmin", err)
	}
	// The demoted admin's principal is stale: the store refuses it.
	if _, err = f.svc.SetUserRole(ctx, admin, carol.User.ID, auth.RoleViewer); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("a demoted admin's stale principal: %v; want ErrForbidden", err)
	}
}

// carolToken signs carol in again and returns the session token.
func carolToken(t *testing.T, f *fixture) string {
	t.Helper()
	res, err := f.svc.Login(context.Background(), ip, "carol", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	return res.Token
}

func countUser(list []*auth.SessionInfo, userID string) int {
	n := 0
	for _, s := range list {
		if s.UserID == userID {
			n++
		}
	}
	return n
}

func TestTwoAdminsDemotingEachOtherOnlyOneWins(t *testing.T) {
	for range 10 {
		f := newFixture(t)
		ctx := context.Background()
		alice := f.session(t, f.setup(t).Token)
		bob := f.signIn(t, alice, "bob", auth.RoleAdmin)

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, pair := range [][2]*auth.Principal{{alice, bob}, {bob, alice}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = f.svc.SetUserRole(ctx, pair[0], pair[1].User.ID, auth.RoleViewer)
			}()
		}
		close(start)
		wg.Wait()

		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrLastAdmin):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		users, err := f.svc.ListUsers(ctx, auth.SystemPrincipal())
		if err != nil {
			t.Fatal(err)
		}
		admins := 0
		for _, u := range users {
			if u.Role == auth.RoleAdmin {
				admins++
			}
		}
		if won != 1 || admins != 1 {
			t.Fatalf("%d demotions succeeded, %d admins left (%v); want exactly one of each", won, admins, errs)
		}
	}
}
