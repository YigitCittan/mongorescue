package auth

import "testing"

func TestEffectiveScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     Method
		role       Role
		key        Scope
		hasCreator bool
		want       Scope
	}{
		{"viewer session", MethodSession, RoleViewer, "", true, ScopeRead},
		{"operator session", MethodSession, RoleOperator, "", true, ScopeOperator},
		{"admin session", MethodSession, RoleAdmin, "", true, ScopeAdmin},
		{"session with an unknown role", MethodSession, Role("root"), "", true, ScopeRead},
		{"session with no role", MethodSession, "", "", true, ScopeRead},
		{"admin key of an admin", MethodAPIKey, RoleAdmin, ScopeAdmin, true, ScopeAdmin},
		{"admin key of an operator", MethodAPIKey, RoleOperator, ScopeAdmin, true, ScopeOperator},
		{"admin key of a viewer", MethodAPIKey, RoleViewer, ScopeAdmin, true, ScopeRead},
		{"operator key of a viewer", MethodAPIKey, RoleViewer, ScopeOperator, true, ScopeRead},
		{"read key of an admin", MethodAPIKey, RoleAdmin, ScopeRead, true, ScopeRead},
		{"operator key of an admin", MethodAPIKey, RoleAdmin, ScopeOperator, true, ScopeOperator},
		{"key of a creator with an unknown role", MethodAPIKey, Role("root"), ScopeAdmin, true, ScopeRead},
		{"key with an unknown scope", MethodAPIKey, RoleAdmin, Scope("super"), true, ScopeRead},
		{"imported admin key", MethodAPIKey, "", ScopeAdmin, false, ScopeAdmin},
		{"imported operator key", MethodAPIKey, "", ScopeOperator, false, ScopeOperator},
		{"system", MethodSystem, "", "", false, ScopeAdmin},
		{"unknown method", Method("other"), RoleAdmin, ScopeAdmin, true, ScopeRead},
	} {
		if got := effectiveScope(tc.method, tc.role, tc.key, tc.hasCreator).Scope; got != tc.want {
			t.Errorf("%s: effective scope %q; want %q", tc.name, got, tc.want)
		}
	}
}

func TestScopeErrorNamesItsSource(t *testing.T) {
	for _, tc := range []struct {
		p    *Principal
		want string
	}{
		{nil, `this request needs the "admin" scope`},
		{&Principal{Method: MethodSession, Role: RoleViewer, Scope: ScopeRead},
			`your role (viewer) has the "read" scope; this request needs "admin"`},
		{&Principal{Method: MethodAPIKey, KeyScope: ScopeOperator, Scope: ScopeOperator},
			`this API key has the "operator" scope; this request needs "admin"`},
		{&Principal{Method: MethodAPIKey, Role: RoleViewer, KeyScope: ScopeAdmin, Scope: ScopeRead},
			`this API key has the "admin" scope, capped at "read" by its creator's role (viewer); this request needs "admin"`},
	} {
		se, ok := tc.p.Require(ScopeAdmin).(*ScopeError)
		if !ok || se.Message() != tc.want || se.Error() != ErrForbidden.Error()+": "+tc.want {
			t.Errorf("%+v: %v; want %q", tc.p, se, tc.want)
		}
	}
}
