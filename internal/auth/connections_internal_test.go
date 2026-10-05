package auth

import (
	"slices"
	"testing"
)

func TestEffectiveConnections(t *testing.T) {
	a, b := OnlyConnections("conn_a"), OnlyConnections("conn_a", "conn_b")
	for _, tc := range []struct {
		name       string
		method     Method
		role       Role
		key        Scope
		hasCreator bool
		user, k    ConnectionSet
		wantIDs    []string // nil: every connection
		wantScope  Scope
	}{
		{"unlimited session", MethodSession, RoleOperator, "", true, nil, nil, nil, ScopeOperator},
		{"limited session", MethodSession, RoleOperator, "", true, a, nil, []string{"conn_a"}, ScopeOperator},
		{"admin session ignores a stored limit", MethodSession, RoleAdmin, "", true, a, nil, nil, ScopeAdmin},
		{"key of an unlimited creator", MethodAPIKey, RoleOperator, ScopeRead, true, nil, a, []string{"conn_a"}, ScopeRead},
		{"unlimited key of a limited creator", MethodAPIKey, RoleOperator, ScopeRead, true, a, nil, []string{"conn_a"}, ScopeRead},
		{"key within its creator", MethodAPIKey, RoleOperator, ScopeOperator, true, b, a, []string{"conn_a"}, ScopeOperator},
		{"key beyond its creator", MethodAPIKey, RoleViewer, ScopeRead, true, OnlyConnections("conn_b"), a, []string{}, ScopeRead},
		{"limited key of an admin", MethodAPIKey, RoleAdmin, ScopeOperator, true, nil, a, []string{"conn_a"}, ScopeOperator},
		{"limited admin key is capped at operator", MethodAPIKey, RoleAdmin, ScopeAdmin, true, nil, a, []string{"conn_a"}, ScopeOperator},
		{"imported key", MethodAPIKey, "", ScopeAdmin, false, nil, nil, nil, ScopeAdmin},
		{"system", MethodSystem, "", "", false, nil, nil, nil, ScopeAdmin},
	} {
		got := effectiveAccess(tc.method, tc.role, tc.key, tc.hasCreator, tc.user, tc.k)
		ids := got.Connections.IDs()
		if (ids == nil) != (tc.wantIDs == nil) || !slices.Equal(ids, tc.wantIDs) || got.Scope != tc.wantScope {
			t.Errorf("%s: %v %q; want %v %q", tc.name, ids, got.Scope, tc.wantIDs, tc.wantScope)
		}
	}
}
