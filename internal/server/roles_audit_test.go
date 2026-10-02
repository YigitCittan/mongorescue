package server

import (
	"net/http"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestAuditLogAnnotatesRoleAndKeyChanges checks the targets the audit log records
// for user creation (role), role changes (role_from, role_to) and key creation
// (scope, ceiling_applied), and that no key material is among them.
func TestAuditLogAnnotatesRoleAndKeyChanges(t *testing.T) {
	f := newAuditLogFixture(t)
	b := f.browser(t)
	b.setup(f.authFixture)
	var bob auth.User
	decodeData(t, b.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": testPassword, "role": "operator"}, nil), &bob)
	if rec := b.do("PUT", "/api/v1/users/"+bob.ID+"/role", map[string]string{"role": "viewer"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("role change: %d %s", rec.Code, rec.Body)
	}
	var key createdAPIKey
	decodeData(t, b.do("POST", "/api/v1/api-keys", map[string]string{"name": "ci", "scope": "operator"}, nil), &key)

	events := f.events(t)
	created := find(events, "POST /api/v1/users")
	changed := find(events, userRoleRoute)
	keys := find(events, createAPIKeyRoute)
	if len(created) != 1 || created[0].Targets[targetRole] != "operator" {
		t.Fatalf("user creation = %+v", created)
	}
	if len(changed) != 1 || changed[0].Targets["id"] != bob.ID || changed[0].Targets[targetRoleFrom] != "operator" ||
		changed[0].Targets[targetRoleTo] != "viewer" {
		t.Fatalf("role change = %+v", changed)
	}
	if len(keys) != 1 || keys[0].Targets[targetScope] != "operator" || keys[0].Targets[targetCeilingApplied] != "true" {
		t.Fatalf("key creation = %+v", keys)
	}
	assertNoBodies(t, events, testPassword, key.Key)
}
