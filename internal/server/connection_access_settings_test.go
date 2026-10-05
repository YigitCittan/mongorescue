package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestSettingsHideMappingConnectionsFromNonAdmins checks that GET /api/v1/settings
// shows the connections of the single sign-on group mappings to administrators
// only; everyone else sees the groups and roles.
func TestSettingsHideMappingConnectionsFromNonAdmins(t *testing.T) {
	f := newAccessFixture(t)
	no := false
	mappings := []settings.OIDCRoleMapping{
		{Group: "team-a", Role: "operator", AllConnections: &no, ConnectionIDs: []string{testConnID}},
		{Group: "team-b", Role: "viewer", AllConnections: &no, ConnectionIDs: []string{accessConnB}},
	}
	if _, err := f.srv.settings.Update(context.Background(), settings.Patch{OIDC: &settings.OIDCPatch{RoleMappings: &mappings}}); err != nil {
		t.Fatal(err)
	}
	code, body := f.get("/api/v1/settings", f.adminKey(t))
	if code != http.StatusOK || !strings.Contains(body, accessConnB) || !strings.Contains(body, `"all_connections":false`) {
		t.Fatalf("admin settings: %d %s; want the mappings' connections", code, body)
	}
	code, body = f.get("/api/v1/settings", f.keyHeaders())
	if code != http.StatusOK || !strings.Contains(body, `"group":"team-b"`) || strings.Contains(body, accessConnB) ||
		strings.Contains(body, testConnID) || strings.Contains(body, "all_connections") {
		t.Fatalf("limited settings: %d %s; want groups and roles only", code, body)
	}
}
