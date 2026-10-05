package settings

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestProtectionsCannotBeLoweredDirectly checks that Update refuses a lower grace
// period and turning the two-person rule off unless the operations service applies
// the change (WithLoweredProtection), and validates the grace period's bounds.
func TestProtectionsCannotBeLoweredDirectly(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, &memRepo{})
	if got := svc.Current().Security.DeleteGraceDays; got != models.DefaultDeleteGraceDays {
		t.Fatalf("default grace = %d", got)
	}
	ten, three, zero, big := 10, 3, 0, 91
	on, off := true, false
	if _, err := svc.Update(ctx, Patch{Security: &SecurityPatch{DeleteGraceDays: &ten, RequireSecondApprover: &on}}); err != nil {
		t.Fatalf("raising = %v", err)
	}
	for name, p := range map[string]*SecurityPatch{
		"lower grace": {DeleteGraceDays: &three},
		"rule off":    {RequireSecondApprover: &off},
	} {
		if _, err := svc.Update(ctx, Patch{Security: p}); !errors.Is(err, ErrProtectionLowered) || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v; want ErrProtectionLowered", name, err)
		}
	}
	for name, v := range map[string]*int{"zero": &zero, "too long": &big} {
		if _, err := svc.Update(WithLoweredProtection(ctx), Patch{Security: &SecurityPatch{DeleteGraceDays: v}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("grace %s = %v; want ErrInvalid", name, err)
		}
	}
	if _, err := svc.Update(WithLoweredProtection(ctx), Patch{Security: &SecurityPatch{DeleteGraceDays: &three, RequireSecondApprover: &off}}); err != nil {
		t.Fatalf("lowering through the operations service = %v", err)
	}
	if s := svc.Current().Security; s.DeleteGraceDays != 3 || s.RequireSecondApprover || s.DeleteGrace() != 3*24*60*60*1e9 {
		t.Fatalf("security = %+v", s)
	}
}

// TestOIDCRevokesAdmin covers the single sign-on changes that can take the admin
// role away and therefore wait for a second administrator.
func TestOIDCRevokesAdmin(t *testing.T) {
	base := OIDC{Enabled: true, Issuer: "https://idp", ClientID: "c", GroupsClaim: "groups",
		RoleMappings: []OIDCRoleMapping{{Group: "admins", Role: "admin"}, {Group: "ops", Role: "operator"}}}
	with := func(fn func(*OIDC)) OIDC {
		o := base
		o.RoleMappings = append([]OIDCRoleMapping(nil), base.RoleMappings...)
		fn(&o)
		return o
	}
	for name, tc := range map[string]struct {
		cur, next  OIDC
		oidcAdmins bool
		want       bool
	}{
		"unchanged":           {base, base, true, false},
		"admin lowered":       {base, with(func(o *OIDC) { o.RoleMappings[0].Role = "viewer" }), false, true},
		"admin removed":       {base, with(func(o *OIDC) { o.RoleMappings = o.RoleMappings[1:] }), false, true},
		"all removed":         {base, with(func(o *OIDC) { o.RoleMappings = nil }), false, true},
		"other issuer":        {base, with(func(o *OIDC) { o.Issuer = "https://other" }), false, true},
		"other groups claim":  {base, with(func(o *OIDC) { o.GroupsClaim = "roles" }), false, true},
		"operator lowered":    {base, with(func(o *OIDC) { o.RoleMappings[1].Role = "viewer" }), false, false},
		"turned off":          {base, with(func(o *OIDC) { o.Enabled = false }), true, false},
		"first mappings":      {OIDC{Enabled: true}, with(func(o *OIDC) { o.RoleMappings = o.RoleMappings[1:] }), true, true},
		"first, no sso admin": {OIDC{Enabled: true}, with(func(o *OIDC) { o.RoleMappings = o.RoleMappings[1:] }), false, false},
	} {
		if got := OIDCRevokesAdmin(tc.cur, tc.next, tc.oidcAdmins); got != tc.want {
			t.Errorf("%s: OIDCRevokesAdmin = %v; want %v", name, got, tc.want)
		}
	}
}
