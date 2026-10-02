package mcp

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestKeyCappedByItsCreatorsRoleListsOnlyReadTools checks that an admin key whose
// creator was demoted to viewer acts as a read key: it lists only the read tools,
// and a call of an operator tool names the cap.
func TestKeyCappedByItsCreatorsRoleListsOnlyReadTools(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	svc, err := auth.NewService(f.store, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	const password = "a long enough password"
	res, err := svc.Setup(ctx, "192.0.2.1", svc.SetupCode(), "admin", password)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := svc.AuthenticateSession(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := svc.CreateUser(ctx, admin, "bob", password, auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	login, err := svc.Login(ctx, "192.0.2.1", "bob", password)
	if err != nil {
		t.Fatal(err)
	}
	bobSession, err := svc.AuthenticateSession(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := svc.CreateAPIKey(ctx, bobSession, "bob's agent", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SetUserRole(ctx, admin, bob.ID, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	p, err := svc.AuthenticateAPIKey(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}

	cs := f.session(t, p)
	names := toolNames(t, cs)
	if len(names) == 0 {
		t.Fatal("a capped key lists no tools; want the read tools")
	}
	for _, name := range names {
		if ToolScopes[name] != auth.ScopeRead {
			t.Errorf("a key capped to read lists %s (%s)", name, ToolScopes[name])
		}
	}
	out := call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"})
	if !out.IsError || !strings.Contains(resultText(out), "capped") {
		t.Fatalf("operator tool with a capped key = %+v; want a refusal naming the cap", out)
	}
}
