package auth_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestMapConnections(t *testing.T) {
	p := auth.OIDCPolicy{RoleMappings: []auth.RoleMapping{
		{Group: "team-a", Role: auth.RoleOperator, ConnectionAccess: only("conn_a")},
		{Group: "team-a2", Role: auth.RoleOperator, ConnectionAccess: only("conn_c")},
		{Group: "team-b", Role: auth.RoleViewer, ConnectionAccess: only("conn_b")},
		{Group: "readers", Role: auth.RoleViewer, ConnectionAccess: auth.EveryConnection()},
		{Group: "ops", Role: auth.RoleOperator, ConnectionAccess: auth.EveryConnection()},
		{Group: "nothing", Role: auth.RoleViewer, ConnectionAccess: only()},
		{Group: "admins", Role: auth.RoleAdmin, ConnectionAccess: auth.EveryConnection()},
	}, DefaultRole: auth.RoleViewer}
	for _, tc := range []struct {
		groups []string
		all    bool
		ids    []string
	}{
		{[]string{"team-a"}, false, []string{"conn_a"}},
		// The union of the mappings that grant the chosen role.
		{[]string{"team-a", "team-a2"}, false, []string{"conn_a", "conn_c"}},
		// A lower role's mappings never widen the connections of the higher role
		// (G1 operator on conn_a, G2 viewer on everything: operator on conn_a).
		{[]string{"team-a", "readers"}, false, []string{"conn_a"}},
		{[]string{"team-a", "team-b"}, false, []string{"conn_a"}},
		{[]string{"team-a", "ops"}, true, nil},
		{[]string{"team-b", "readers"}, true, nil},
		// An empty list is none.
		{[]string{"nothing"}, false, []string{}},
		{[]string{"team-a", "admins"}, true, nil},
		// The default role (no mapping matched) gets every connection.
		{[]string{"unmapped"}, true, nil},
	} {
		got := p.MapConnections(tc.groups, p.MapRole(tc.groups))
		if got.AllConnections != tc.all || !slices.Equal(got.ConnectionIDs, tc.ids) {
			t.Errorf("groups %v: %+v; want all %v, %v", tc.groups, got, tc.all, tc.ids)
		}
	}
}

// grantGate holds every admin grant and records the requests.
type grantGate struct{ requests []auth.AdminGrant }

func (g *grantGate) HoldsAdminGrants(context.Context) bool { return true }
func (g *grantGate) RequestAdminGrant(_ context.Context, a auth.AdminGrant) error {
	g.requests = append(g.requests, a)
	return auth.ErrAwaitingApproval
}
func (g *grantGate) RequestSignInDemotion(_ context.Context, a auth.AdminGrant) error {
	g.requests = append(g.requests, a)
	return nil
}

func TestOIDCMappingsDecideConnections(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	f.policy.RoleMappings = []auth.RoleMapping{
		{Group: "team-a", Role: auth.RoleOperator, ConnectionAccess: only("conn_a")},
		{Group: "team-b", Role: auth.RoleOperator, ConnectionAccess: only("conn_b")},
		{Group: "ops", Role: auth.RoleOperator, ConnectionAccess: auth.EveryConnection()},
		{Group: "readers", Role: auth.RoleViewer, ConnectionAccess: auth.EveryConnection()},
		{Group: "admins", Role: auth.RoleAdmin, ConnectionAccess: auth.EveryConnection()},
	}
	// The two-person rule holds admin grants, never a change of connections.
	gate := &grantGate{}
	f.svc.SetAdminGrantGate(gate)

	login := func(groups ...string) *auth.Principal {
		t.Helper()
		res, err := f.svc.LoginOIDC(ctx, identity("7", groups...))
		if err != nil {
			t.Fatalf("sign-in with %v: %v", groups, err)
		}
		return f.session(t, res.Token)
	}
	if p := login("team-a"); !slices.Equal(p.Connections.IDs(), []string{"conn_a"}) {
		t.Fatalf("team-a: %v", p.Connections.IDs())
	}
	if p := login("team-a", "team-b"); !slices.Equal(p.Connections.IDs(), []string{"conn_a", "conn_b"}) {
		t.Fatalf("team-a and team-b: %v; want the union", p.Connections.IDs())
	}
	// A viewer mapping on every connection does not widen the operator's.
	if p := login("team-a", "readers"); p.Role != auth.RoleOperator || !slices.Equal(p.Connections.IDs(), []string{"conn_a"}) {
		t.Fatalf("team-a and readers: %s %v; want operator on conn_a", p.Role, p.Connections.IDs())
	}
	// Widening to every connection needs no approval: it is not an admin grant.
	if p := login("team-a", "ops"); p.Connections.Limited() {
		t.Fatalf("team-a and ops: %v; want every connection", p.Connections.IDs())
	}
	if p := login("team-b"); !slices.Equal(p.Connections.IDs(), []string{"conn_b"}) {
		t.Fatalf("back to team-b: %v", p.Connections.IDs())
	}
	if len(gate.requests) != 0 {
		t.Fatalf("approval requests %+v; want none for connection changes", gate.requests)
	}
	// Mapped connections are the provider's: an admin cannot change them by hand.
	users, err := f.svc.ListUsers(ctx, auth.SystemPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == "user-7" {
			if _, err = f.svc.SetUserConnections(ctx, auth.SystemPrincipal(), u.ID, auth.EveryConnection()); err == nil {
				t.Fatal("hand-set connections of a mapped single sign-on user; want ErrRoleManagedByProvider")
			}
		}
	}
	// SetUserConnections lifts a local user's limit without approval too.
	admin := f.session(t, f.admin.Token)
	u, err := f.svc.CreateUserWithConnections(ctx, admin, "local-op", adminPassword, auth.RoleOperator, only("conn_a"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err = f.svc.SetUserConnections(ctx, admin, u.ID, auth.EveryConnection()); err != nil || !u.AllConnections || len(gate.requests) != 0 {
		t.Fatalf("lift a local limit with the two-person rule on: %+v, %v, requests %+v", u, err, gate.requests)
	}
}
