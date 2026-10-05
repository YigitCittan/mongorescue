package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestCheckApprover(t *testing.T) {
	alice := &auth.User{ID: "usr_alice", Username: "alice", Role: auth.RoleAdmin}
	bob := &auth.User{ID: "usr_bob", Username: "bob", Role: auth.RoleAdmin}
	session := func(u *auth.User, scope auth.Scope) *auth.Principal {
		return &auth.Principal{User: u, Method: auth.MethodSession, Scope: scope, Role: u.Role}
	}
	for _, c := range []struct {
		name      string
		p         *auth.Principal
		requester string
		want      error
	}{
		{"no principal", nil, "usr_alice", auth.ErrApprovalNeedsSession},
		{"api key of another admin", &auth.Principal{User: bob, Method: auth.MethodAPIKey, Scope: auth.ScopeAdmin}, "usr_alice", auth.ErrApprovalNeedsSession},
		{"system", auth.SystemPrincipal(), "usr_alice", auth.ErrApprovalNeedsSession},
		{"requester", session(alice, auth.ScopeAdmin), "usr_alice", auth.ErrSelfApproval},
		{"operator", session(&auth.User{ID: "usr_op", Role: auth.RoleOperator}, auth.ScopeOperator), "usr_alice", auth.ErrForbidden},
		{"another admin", session(bob, auth.ScopeAdmin), "usr_alice", nil},
		{"any admin for a key without a user", session(alice, auth.ScopeAdmin), "", nil},
	} {
		if err := auth.CheckApprover(c.p, c.requester); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: %v; want %v", c.name, err, c.want)
		}
	}
}

func TestCheckSecondApproverPossible(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.setup(t)
	svc := f.svc
	if err := svc.CheckSecondApproverPossible(ctx); !errors.Is(err, auth.ErrTooFewAdmins) {
		t.Fatalf("one admin = %v; want ErrTooFewAdmins", err)
	}
	if _, err := svc.CreateUser(ctx, auth.SystemPrincipal(), "viewer", adminPassword, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckSecondApproverPossible(ctx); !errors.Is(err, auth.ErrTooFewAdmins) {
		t.Fatalf("one admin and a viewer = %v; want ErrTooFewAdmins", err)
	}
	if _, err := svc.CreateUser(ctx, auth.SystemPrincipal(), "second", adminPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckSecondApproverPossible(ctx); err != nil {
		t.Fatalf("two admins = %v", err)
	}
}
