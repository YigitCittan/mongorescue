package settings

import (
	"encoding/json"
	"testing"
)

// TestOIDCMappingAccessIsExplicit checks that an empty connection list of a group
// mapping is none, never every connection: only all_connections (or a mapping
// stored before connection access, with neither field) grants every connection.
func TestOIDCMappingAccessIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		all  bool
		none bool
	}{
		{`{"group":"g","role":"viewer"}`, true, false},
		{`{"group":"g","role":"viewer","all_connections":true}`, true, false},
		{`{"group":"g","role":"viewer","connection_ids":[]}`, false, true},
		{`{"group":"g","role":"viewer","all_connections":false}`, false, true},
		{`{"group":"g","role":"viewer","all_connections":false,"connection_ids":["c1"]}`, false, false},
	} {
		var m OIDCRoleMapping
		if err := json.Unmarshal([]byte(tc.raw), &m); err != nil {
			t.Fatal(err)
		}
		a := m.Access()
		if a.AllConnections != tc.all || (len(a.ConnectionIDs) == 0 && !a.AllConnections) != tc.none {
			t.Errorf("%s: %+v", tc.raw, a)
		}
		// Validation stores the flag explicitly.
		o := OIDC{RoleMappings: []OIDCRoleMapping{m}}
		if err := validateOIDC(&o); err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		got := o.RoleMappings[0]
		if got.AllConnections == nil || *got.AllConnections != tc.all {
			t.Errorf("%s: stored %+v; want all_connections %v", tc.raw, got, tc.all)
		}
	}
}
