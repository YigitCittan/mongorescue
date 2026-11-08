package operations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestTurningOffTheOutboundSettingsNeedsApproval proves that under the two-person
// rule turning security.require_https_endpoints or security.outbound_deny_private
// off waits for a second administrator (never the requester), and that turning them
// on is immediate.
func TestTurningOffTheOutboundSettingsNeedsApproval(t *testing.T) {
	env := newAdminEnv(t)
	aliceCtx := env.ctxOf(t, "alice")
	on, off := true, false
	bothOn := settings.Patch{Security: &settings.SecurityPatch{RequireHTTPSEndpoints: &on, OutboundDenyPrivate: &on}}
	res, err := env.svc.UpdateSettings(aliceCtx, bothOn)
	if err != nil || len(res.Approvals) != 0 {
		t.Fatalf("turning them on = %+v, %v; want no approval", res, err)
	}
	if s := env.settings.Current().Security; !s.RequireHTTPSEndpoints || !s.OutboundDenyPrivate {
		t.Fatalf("security = %+v; want both on at once", s)
	}
	for name, p := range map[string]*settings.SecurityPatch{
		"https":   {RequireHTTPSEndpoints: &off},
		"private": {OutboundDenyPrivate: &off},
		"both":    {RequireHTTPSEndpoints: &off, OutboundDenyPrivate: &off},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := env.settings.UpdateChanged(settings.WithLoweredProtection(context.Background()), bothOn); err != nil {
				t.Fatal(err)
			}
			res, err := env.svc.UpdateSettings(aliceCtx, settings.Patch{Security: p})
			if err != nil || len(res.Approvals) != 1 || res.Approvals[0].Action != models.ApprovalLowerOutbound {
				t.Fatalf("turning off = %+v, %v; want one approval request", res, err)
			}
			if s := env.settings.Current().Security; !s.RequireHTTPSEndpoints || !s.OutboundDenyPrivate {
				t.Fatalf("security = %+v; want both still on", s)
			}
			if _, err = env.svc.Approve(aliceCtx, res.Approvals[0].ID); !errors.Is(err, operations.ErrSelfApproval) {
				t.Fatalf("the requester approving = %v; want ErrSelfApproval", err)
			}
			if _, err = env.svc.Approve(env.ctxOf(t, "bob"), res.Approvals[0].ID); err != nil {
				t.Fatal(err)
			}
			s := env.settings.Current().Security
			if s.RequireHTTPSEndpoints != (p.RequireHTTPSEndpoints == nil) || s.OutboundDenyPrivate != (p.OutboundDenyPrivate == nil) {
				t.Fatalf("security after approval = %+v; want only the requested setting(s) off", s)
			}
		})
	}
}

// TestOutboundSettingsChangeAtOnceWithoutTheRule proves the settings are not held
// while the two-person rule is off.
func TestOutboundSettingsChangeAtOnceWithoutTheRule(t *testing.T) {
	env := newProtEnv(t)
	ctx := asUser("alice", auth.ScopeAdmin)
	on, off := true, false
	for _, p := range []*settings.SecurityPatch{
		{RequireHTTPSEndpoints: &on, OutboundDenyPrivate: &on},
		{RequireHTTPSEndpoints: &off, OutboundDenyPrivate: &off},
	} {
		res, err := env.svc.UpdateSettings(ctx, settings.Patch{Security: p})
		if err != nil || len(res.Approvals) != 0 {
			t.Fatalf("update = %+v, %v; want no approval", res, err)
		}
		if s := env.settings.Current().Security; s.RequireHTTPSEndpoints != *p.RequireHTTPSEndpoints || s.OutboundDenyPrivate != *p.OutboundDenyPrivate {
			t.Fatalf("security = %+v", s)
		}
	}
}
