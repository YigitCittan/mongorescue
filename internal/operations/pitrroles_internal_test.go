package operations

import (
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/connections"
)

func TestPITRRoleCheck(t *testing.T) {
	adminRole := func(names ...string) []connections.Role {
		var out []connections.Role
		for _, n := range names {
			out = append(out, connections.Role{Role: n, DB: "admin"})
		}
		return out
	}
	for name, c := range map[string]struct {
		u       connections.UserRoles
		missing []string
		custom  bool
	}{
		"no access control": {u: connections.UserRoles{}},
		"anyAction":         {u: connections.UserRoles{AuthEnabled: true, AnyAction: true}},
		"root":              {u: connections.UserRoles{AuthEnabled: true, Roles: adminRole("root")}},
		"the three roles":   {u: connections.UserRoles{AuthEnabled: true, Roles: adminRole("restore", "readWriteAnyDatabase", "dbAdminAnyDatabase")}},
		"restore only": {u: connections.UserRoles{AuthEnabled: true, Roles: adminRole("restore")},
			missing: []string{"readWriteAnyDatabase", "dbAdminAnyDatabase"}},
		"roles in another database": {u: connections.UserRoles{AuthEnabled: true, Roles: []connections.Role{{Role: "restore", DB: "shop"}}},
			missing: []string{"restore", "readWriteAnyDatabase", "dbAdminAnyDatabase"}},
		"custom role": {u: connections.UserRoles{AuthEnabled: true, Roles: adminRole("restore", "pitrReplay")},
			missing: []string{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, custom: true},
	} {
		missing, custom := pitrRoleCheck(c.u)
		if !slices.Equal(missing, c.missing) || custom != c.custom {
			t.Errorf("%s: missing %v custom %v; want %v %v", name, missing, custom, c.missing, c.custom)
		}
	}
}
