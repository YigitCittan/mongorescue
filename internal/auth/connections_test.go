package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestConnectionSet(t *testing.T) {
	var all auth.ConnectionSet
	if all.Limited() || !all.Allows("conn_a") || !all.Allows("") || all.IDs() != nil {
		t.Fatal("a nil set allows every connection")
	}
	if auth.NewConnectionSet(nil) != nil {
		t.Fatal("no IDs is every connection")
	}
	a := auth.OnlyConnections("conn_a")
	if !a.Limited() || !a.Allows("conn_a") || a.Allows("conn_b") || a.Allows("") {
		t.Fatal("a limited set allows only its members, never the empty ID")
	}
	none := auth.OnlyConnections()
	if !none.Limited() || none.Allows("conn_a") || none.IDs() == nil || len(none.IDs()) != 0 {
		t.Fatal("an empty limited set allows nothing and lists []")
	}
	ab := auth.OnlyConnections("conn_b", "conn_a")
	if got := ab.IDs(); !slices.Equal(got, []string{"conn_a", "conn_b"}) {
		t.Fatalf("IDs = %v; want sorted", got)
	}
	if got := ab.Intersect(a).IDs(); !slices.Equal(got, []string{"conn_a"}) {
		t.Fatalf("intersect = %v", got)
	}
	if got := all.Intersect(a).IDs(); !slices.Equal(got, []string{"conn_a"}) {
		t.Fatalf("all ∩ a = %v", got)
	}
	if all.Intersect(nil).Limited() {
		t.Fatal("all ∩ all is all")
	}
	if got := a.Intersect(auth.OnlyConnections("conn_b")); !got.Limited() || len(got) != 0 {
		t.Fatalf("disjoint intersection = %v; want limited to none", got)
	}
	switch {
	case !all.Covers(a), !ab.Covers(a), a.Covers(ab), a.Covers(nil), !a.Covers(none):
		t.Fatal("Covers is the subset relation, with nil as every connection")
	}
	var nilPrincipal *auth.Principal
	if nilPrincipal.AllowsConnection("conn_a") {
		t.Fatal("a nil principal allows nothing")
	}
	if !auth.ConnectionAllowed(context.Background(), "conn_a") {
		t.Fatal("a context without a principal is the application itself")
	}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Connections: a})
	if auth.ConnectionAllowed(ctx, "conn_b") || !auth.ConnectionAllowed(ctx, "conn_a") {
		t.Fatal("ConnectionAllowed follows the principal in the context")
	}
}

func TestNormalizeConnectionIDs(t *testing.T) {
	got, err := auth.NormalizeConnectionIDs([]string{" conn_b ", "conn_a", "conn_b"})
	if err != nil || !slices.Equal(got, []string{"conn_a", "conn_b"}) {
		t.Fatalf("normalize = %v, %v", got, err)
	}
	if got, err = auth.NormalizeConnectionIDs(nil); err != nil || got != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, bad := range [][]string{{""}, {"a\nb"}, {string(make([]byte, 200))}, make([]string, auth.MaxConnectionIDs+1)} {
		if _, err = auth.NormalizeConnectionIDs(bad); !errors.Is(err, auth.ErrInvalidConnections) {
			t.Errorf("normalize %q: err = %v; want ErrInvalidConnections", bad, err)
		}
	}
}

func TestUserConnectionsLimitSessionsAndKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	op := f.signIn(t, admin, "oncall", auth.RoleOperator)
	if op.Connections.Limited() {
		t.Fatal("a new user reaches every connection")
	}
	// The operator's unlimited key, created before the limit.
	_, wide, err := f.svc.CreateAPIKey(ctx, op, "wide", auth.ScopeOperator)
	if err != nil {
		t.Fatal(err)
	}

	u, err := f.svc.SetUserConnections(ctx, admin, op.User.ID, []string{"conn_a"})
	if err != nil || !slices.Equal(u.ConnectionIDs, []string{"conn_a"}) {
		t.Fatalf("SetUserConnections = %+v, %v", u, err)
	}
	res, err := f.svc.Login(ctx, ip, "oncall", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	p := f.session(t, res.Token)
	if !p.AllowsConnection("conn_a") || p.AllowsConnection("conn_b") || p.Scope != auth.ScopeOperator {
		t.Fatalf("limited session = %+v", p)
	}
	// The creator's limit caps the key created before it, on the next request.
	kp, err := f.svc.AuthenticateAPIKey(ctx, wide)
	if err != nil || !kp.AllowsConnection("conn_a") || kp.AllowsConnection("conn_b") {
		t.Fatalf("key of a limited creator = %+v, %v", kp, err)
	}

	// A limited user cannot mint a key beyond their connections, nor an unlimited one.
	if _, _, err = f.svc.CreateAPIKeyWithConnections(ctx, p, "beyond", auth.ScopeRead, []string{"conn_b"}); !errors.Is(err, auth.ErrConnectionsExceedAccess) {
		t.Fatalf("key beyond the creator: err = %v; want ErrConnectionsExceedAccess", err)
	}
	k, plain, err := f.svc.CreateAPIKeyWithConnections(ctx, p, "inherits", auth.ScopeRead, nil)
	if err != nil || !slices.Equal(k.EffectiveConnectionIDs, []string{"conn_a"}) {
		t.Fatalf("key without a limit = %+v, %v; want the creator's connections", k, err)
	}
	if kp, err = f.svc.AuthenticateAPIKey(ctx, plain); err != nil || !kp.Connections.Limited() || kp.AllowsConnection("conn_b") {
		t.Fatalf("inherited key = %+v, %v", kp, err)
	}

	// Lifting the user's limit lifts the cap of their keys.
	if _, err = f.svc.SetUserConnections(ctx, admin, op.User.ID, nil); err != nil {
		t.Fatal(err)
	}
	if kp, err = f.svc.AuthenticateAPIKey(ctx, plain); err != nil || kp.Connections.Limited() {
		t.Fatalf("key after lifting the limit = %+v, %v", kp, err)
	}
	keys, err := f.svc.ListAPIKeys(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.EffectiveConnectionIDs != nil {
			t.Errorf("key %s effective connections = %v; want every connection", key.Name, key.EffectiveConnectionIDs)
		}
	}
}

func TestAdminsAreNeverLimitedToConnections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.session(t, f.setup(t).Token)
	bob := f.signIn(t, admin, "bob", auth.RoleAdmin)
	if _, err := f.svc.SetUserConnections(ctx, admin, bob.User.ID, []string{"conn_a"}); !errors.Is(err, auth.ErrAdminConnections) {
		t.Fatalf("limit an admin: err = %v; want ErrAdminConnections", err)
	}
	if _, err := f.svc.CreateUserWithConnections(ctx, admin, "carol", adminPassword, auth.RoleAdmin, []string{"conn_a"}); !errors.Is(err, auth.ErrAdminConnections) {
		t.Fatalf("create a limited admin: err = %v; want ErrAdminConnections", err)
	}
	if _, _, err := f.svc.CreateAPIKeyWithConnections(ctx, admin, "ci", auth.ScopeAdmin, []string{"conn_a"}); !errors.Is(err, auth.ErrAdminConnections) {
		t.Fatalf("limited admin key: err = %v; want ErrAdminConnections", err)
	}
	// An admin may create a limited key below admin.
	k, plain, err := f.svc.CreateAPIKeyWithConnections(ctx, admin, "team a", auth.ScopeOperator, []string{"conn_a"})
	if err != nil || !slices.Equal(k.ConnectionIDs, []string{"conn_a"}) {
		t.Fatalf("limited operator key = %+v, %v", k, err)
	}
	if p, authErr := f.svc.AuthenticateAPIKey(ctx, plain); authErr != nil || p.AllowsConnection("conn_b") || p.Scope != auth.ScopeOperator {
		t.Fatalf("limited key = %+v, %v", p, authErr)
	}

	// Promoting a limited user lifts the limit; non-admins may not set limits.
	dave, err := f.svc.CreateUserWithConnections(ctx, admin, "dave", adminPassword, auth.RoleViewer, []string{"conn_a"})
	if err != nil || !slices.Equal(dave.ConnectionIDs, []string{"conn_a"}) {
		t.Fatalf("limited viewer = %+v, %v", dave, err)
	}
	op := f.signIn(t, admin, "erin", auth.RoleOperator)
	if _, err = f.svc.SetUserConnections(ctx, op, dave.ID, nil); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator sets connections: err = %v; want ErrForbidden", err)
	}
	if _, err = f.svc.SetUserRole(ctx, admin, dave.ID, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Login(ctx, ip, "dave", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if p := f.session(t, res.Token); p.Connections.Limited() || len(p.User.ConnectionIDs) != 0 || p.Scope != auth.ScopeAdmin {
		t.Fatalf("promoted user = %+v", p)
	}
	if _, err = f.svc.SetUserConnections(ctx, admin, "usr_missing", nil); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("unknown user: err = %v", err)
	}
}
