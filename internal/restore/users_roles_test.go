package restore

import (
	"errors"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestBuildRestoreArgsUsersAndRoles(t *testing.T) {
	e := NewEngine(nil, "mongodb://localhost")
	no := false
	inPlace := models.RestoreRequest{BackupID: "bkp_1", SafeClone: &no, ConfirmInPlace: true, RestoreUsersAndRoles: true}
	for _, tc := range []struct {
		name   string
		req    models.RestoreRequest
		target string
		want   []string
	}{
		{"in place", inPlace, "shop",
			[]string{"--config=x", "--archive", "--gzip", "--db=shop", "--restoreDbUsersAndRoles", "--nsInclude=shop.*"}},
		{"in place, not requested", models.RestoreRequest{BackupID: "bkp_1", SafeClone: &no, ConfirmInPlace: true}, "shop",
			[]string{"--config=x", "--archive", "--gzip", "--nsInclude=shop.*"}},
		// Never combined with a rename, even if validation were bypassed.
		{"renamed target", inPlace, "other",
			[]string{"--config=x", "--archive", "--gzip", "--nsFrom=shop.*", "--nsTo=other.*", "--nsInclude=shop.*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.buildRestoreArgs("--config=x", "shop", tc.target, tc.req, true); !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q\nwant  %q", got, tc.want)
			}
		})
	}
}

// TestUsersAndRolesValidationMatrix covers every combination of restore target and
// backup content for restore_users_and_roles, through the engine's Prepare.
func TestUsersAndRolesValidationMatrix(t *testing.T) {
	e := NewEngine(nil, "mongodb://localhost")
	no, yes := false, true
	withUsers := &models.BackupRecord{ID: "bkp_1", Database: "shop", UsersAndRoles: true}
	without := &models.BackupRecord{ID: "bkp_2", Database: "shop"}
	admin := &models.BackupRecord{ID: "bkp_3", Database: "admin", UsersAndRoles: true}
	for _, tc := range []struct {
		name   string
		req    models.RestoreRequest
		source *models.BackupRecord
		want   error
	}{
		{"not requested, safe clone", models.RestoreRequest{}, without, nil},
		{"safe clone (default)", models.RestoreRequest{RestoreUsersAndRoles: true}, withUsers, models.ErrUsersAndRolesNotAllowed},
		{"explicit safe clone", models.RestoreRequest{SafeClone: &yes, RestoreUsersAndRoles: true}, withUsers, models.ErrUsersAndRolesNotAllowed},
		{"in place", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, RestoreUsersAndRoles: true}, withUsers, nil},
		{"in place, target is the source", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, TargetDatabase: "shop", RestoreUsersAndRoles: true}, withUsers, nil},
		{"in place, renamed", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, TargetDatabase: "other", RestoreUsersAndRoles: true}, withUsers, models.ErrUsersAndRolesNotAllowed},
		{"in place, backup without users", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, RestoreUsersAndRoles: true}, without, models.ErrNoUsersAndRoles},
		{"in place, admin database", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, RestoreUsersAndRoles: true}, admin, models.ErrUsersAndRolesNotAllowed},
		{"in place, not requested, backup without users", models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true}, without, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.BackupID = tc.source.ID
			rec, err := e.Prepare(tc.req, tc.source)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Prepare = %v; want nil", err)
				}
				if rec.UsersAndRoles != tc.req.RestoreUsersAndRoles {
					t.Fatalf("record users_and_roles = %v; want %v", rec.UsersAndRoles, tc.req.RestoreUsersAndRoles)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Prepare = %v; want %v", err, tc.want)
			}
		})
	}
}
