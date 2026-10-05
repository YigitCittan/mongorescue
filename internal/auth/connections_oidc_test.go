package auth_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestMapConnections(t *testing.T) {
	p := auth.OIDCPolicy{RoleMappings: []auth.RoleMapping{
		{Group: "team-a", Role: auth.RoleOperator, ConnectionIDs: []string{"conn_a"}},
		{Group: "team-b", Role: auth.RoleViewer, ConnectionIDs: []string{"conn_b", "conn_a"}},
		{Group: "everyone", Role: auth.RoleViewer},
		{Group: "admins", Role: auth.RoleAdmin},
	}, DefaultRole: auth.RoleViewer}
	for _, tc := range []struct {
		groups []string
		want   []string
	}{
		{[]string{"team-a"}, []string{"conn_a"}},
		{[]string{"team-a", "team-b"}, []string{"conn_a", "conn_b"}},
		{[]string{"team-a", "everyone"}, nil},
		{[]string{"team-a", "admins"}, nil},
		{[]string{"unmapped"}, nil},
	} {
		got := p.MapConnections(tc.groups, p.MapRole(tc.groups))
		if !slices.Equal(got, tc.want) {
			t.Errorf("groups %v: connections %v; want %v", tc.groups, got, tc.want)
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
		{Group: "team-a", Role: auth.RoleOperator, ConnectionIDs: []string{"conn_a"}},
		{Group: "team-b", Role: auth.RoleOperator, ConnectionIDs: []string{"conn_b"}},
		{Group: "ops", Role: auth.RoleOperator},
		{Group: "admins", Role: auth.RoleAdmin},
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
			if _, err = f.svc.SetUserConnections(ctx, auth.SystemPrincipal(), u.ID, nil); err == nil {
				t.Fatal("hand-set connections of a mapped single sign-on user; want ErrRoleManagedByProvider")
			}
		}
	}
	// SetUserConnections lifts a local user's limit without approval too.
	admin := f.session(t, f.admin.Token)
	u, err := f.svc.CreateUserWithConnections(ctx, admin, "local-op", adminPassword, auth.RoleOperator, []string{"conn_a"})
	if err != nil {
		t.Fatal(err)
	}
	if u, err = f.svc.SetUserConnections(ctx, admin, u.ID, nil); err != nil || len(u.ConnectionIDs) != 0 || len(gate.requests) != 0 {
		t.Fatalf("lift a local limit with the two-person rule on: %+v, %v, requests %+v", u, err, gate.requests)
	}
}
