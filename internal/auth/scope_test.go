package auth_test

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestScopeAllows(t *testing.T) {
	cases := []struct {
		have, need auth.Scope
		want       bool
	}{
		{auth.ScopeRead, auth.ScopeRead, true},
		{auth.ScopeRead, auth.ScopeOperator, false},
		{auth.ScopeRead, auth.ScopeAdmin, false},
		{auth.ScopeOperator, auth.ScopeRead, true},
		{auth.ScopeOperator, auth.ScopeOperator, true},
		{auth.ScopeOperator, auth.ScopeAdmin, false},
		{auth.ScopeAdmin, auth.ScopeRead, true},
		{auth.ScopeAdmin, auth.ScopeOperator, true},
		{auth.ScopeAdmin, auth.ScopeAdmin, true},
		{"", auth.ScopeRead, false},
		{"root", auth.ScopeRead, false},
		{auth.ScopeAdmin, "root", false},
	}
	for _, tc := range cases {
		if got := tc.have.Allows(tc.need); got != tc.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", tc.have, tc.need, got, tc.want)
		}
	}
}

func TestParseScope(t *testing.T) {
	for _, s := range auth.Scopes() {
		got, err := auth.ParseScope(string(s))
		if err != nil || got != s {
			t.Fatalf("ParseScope(%q) = %q, %v", s, got, err)
		}
	}
	if got, err := auth.ParseScope(""); err != nil || got != auth.ScopeRead {
		t.Fatalf(`ParseScope("") = %q, %v; want read`, got, err)
	}
	if _, err := auth.ParseScope("superuser"); !errors.Is(err, auth.ErrInvalidScope) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestPrincipalRequire(t *testing.T) {
	var none *auth.Principal
	if none.Allows(auth.ScopeRead) {
		t.Fatal("a nil principal must allow nothing")
	}
	err := none.Require(auth.ScopeRead)
	var se *auth.ScopeError
	if !errors.As(err, &se) || !errors.Is(err, auth.ErrForbidden) || se.Need != auth.ScopeRead {
		t.Fatalf("nil principal: %v", err)
	}

	op := &auth.Principal{Method: auth.MethodAPIKey, Scope: auth.ScopeOperator}
	if err = op.Require(auth.ScopeOperator); err != nil {
		t.Fatal(err)
	}
	err = op.Require(auth.ScopeAdmin)
	if !errors.As(err, &se) || se.Have != auth.ScopeOperator || se.Need != auth.ScopeAdmin {
		t.Fatalf("operator requiring admin: %v", err)
	}

	ctx := context.Background()
	if err = auth.RequireScope(ctx, auth.ScopeRead); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("a context without a principal must be refused: %v", err)
	}
	if err = auth.RequireScope(auth.WithPrincipal(ctx, auth.SystemPrincipal()), auth.ScopeAdmin); err != nil {
		t.Fatalf("the system principal is admin: %v", err)
	}
}

func TestAPIKeyScopes(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	admin := f.session(t, res.Token)

	if _, _, err := f.svc.CreateAPIKey(ctx, admin, "bad", "root"); !errors.Is(err, auth.ErrInvalidScope) {
		t.Fatalf("unknown scope: %v", err)
	}
	for _, scope := range auth.Scopes() {
		k, plain, err := f.svc.CreateAPIKey(ctx, admin, "key "+string(scope), scope)
		if err != nil || k.Scope != scope {
			t.Fatalf("create %q: %+v, %v", scope, k, err)
		}
		stored, err := f.store.GetAPIKeyByPrefix(ctx, k.Prefix)
		if err != nil || stored.Scope != scope {
			t.Fatalf("stored scope of %q = %+v, %v", scope, stored, err)
		}
		p, err := f.svc.AuthenticateAPIKey(ctx, plain)
		if err != nil || p.Scope != scope || p.APIKeyName != "key "+string(scope) {
			t.Fatalf("principal of %q = %+v, %v", scope, p, err)
		}
	}

	// A key imported from MONGORESCUE_API_KEY had full rights and keeps them.
	const static = "static-env-key-0123456789"
	if _, err := f.svc.ImportAPIKey(ctx, static); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.AuthenticateAPIKey(ctx, static)
	if err != nil || p.Scope != auth.ScopeAdmin {
		t.Fatalf("imported key = %+v, %v; want admin", p, err)
	}
}

// unknownScopeRepo reports a scope this build does not know for every key.
type unknownScopeRepo struct{ auth.Repository }

func (r unknownScopeRepo) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*auth.APIKey, error) {
	k, err := r.Repository.GetAPIKeyByPrefix(ctx, prefix)
	if err == nil {
		k.Scope = "superuser"
	}
	return k, err
}

func TestUnknownStoredScopeFailsClosed(t *testing.T) {
	f := newFixture(t)
	res := f.setup(t)
	ctx := context.Background()
	_, plain, err := f.svc.CreateAPIKey(ctx, f.session(t, res.Token), "future", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(unknownScopeRepo{f.store}, auth.WithBcryptCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.AuthenticateAPIKey(ctx, plain)
	if err != nil || p.Scope != auth.ScopeRead {
		t.Fatalf("unknown stored scope = %+v, %v; want read", p, err)
	}
}
