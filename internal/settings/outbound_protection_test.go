package settings

import (
	"context"
	"errors"
	"testing"
)

// TestOutboundSettingsLoweringNeedsTheOperationsService checks that, with the
// two-person rule on, turning security.require_https_endpoints or
// security.outbound_deny_private off is refused outside WithLoweredProtection, that
// turning them on is not, and that without the rule they change freely.
func TestOutboundSettingsLoweringNeedsTheOperationsService(t *testing.T) {
	ctx := context.Background()
	on, off := true, false
	for name, set := range map[string]func(v *bool) *SecurityPatch{
		"require_https_endpoints": func(v *bool) *SecurityPatch { return &SecurityPatch{RequireHTTPSEndpoints: v} },
		"outbound_deny_private":   func(v *bool) *SecurityPatch { return &SecurityPatch{OutboundDenyPrivate: v} },
	} {
		t.Run(name, func(t *testing.T) {
			svc := newSvc(t, &memRepo{})
			if _, err := svc.Update(ctx, Patch{Security: set(&on)}); err != nil {
				t.Fatalf("turn on = %v", err)
			}
			if _, err := svc.Update(ctx, Patch{Security: set(&off)}); err != nil {
				t.Fatalf("turn off without the two-person rule = %v", err)
			}
			if _, err := svc.Update(ctx, Patch{Security: set(&on)}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Update(ctx, Patch{Security: &SecurityPatch{RequireSecondApprover: &on}}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Update(ctx, Patch{Security: set(&on)}); err != nil {
				t.Fatalf("turn on again with the rule = %v", err)
			}
			if _, err := svc.Update(ctx, Patch{Security: set(&off)}); !errors.Is(err, ErrProtectionLowered) || !errors.Is(err, ErrInvalid) {
				t.Fatalf("turn off with the rule = %v; want ErrProtectionLowered", err)
			}
			if _, err := svc.Update(WithLoweredProtection(ctx), Patch{Security: set(&off)}); err != nil {
				t.Fatalf("turn off through the operations service = %v", err)
			}
			if _, err := svc.Update(ctx, Patch{Security: set(&off)}); err != nil {
				t.Fatalf("setting it off again while already off = %v", err)
			}
		})
	}
}
