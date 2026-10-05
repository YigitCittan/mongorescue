package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProtectionLowered is returned by Update for a change that lowers a delete
// protection directly: security.delete_grace_days lowered,
// security.require_second_approver turned off, or (with it on) an oidc change that
// can grant admin. Those changes go through the
// operations service, which delays them by the current grace period or asks a second
// administrator (see operations.UpdateSettings); it applies them with
// WithLoweredProtection once they are due or approved. It wraps ErrInvalid.
var ErrProtectionLowered = fmt.Errorf("%w: lowering a delete protection must be requested, not set", ErrInvalid)

type loweringKey struct{}

// WithLoweredProtection returns a context in which Update may lower the delete
// protections. Only the operations service uses it, for changes that waited for the
// grace period or were approved by a second administrator.
func WithLoweredProtection(ctx context.Context) context.Context {
	return context.WithValue(ctx, loweringKey{}, true)
}

// loweringAllowed reports whether ctx came from WithLoweredProtection.
func loweringAllowed(ctx context.Context) bool {
	ok, _ := ctx.Value(loweringKey{}).(bool)
	return ok
}

// LowersProtection reports which delete protections next lowers compared with cur:
// a shorter grace period, and the two-person rule turned off.
func LowersProtection(cur, next Security) (grace, secondApprover bool) {
	return next.DeleteGraceDays < cur.DeleteGraceDays, cur.RequireSecondApprover && !next.RequireSecondApprover
}

// OIDCGrantsAdmin reports whether next can grant the admin role through single
// sign-on where cur could not: next is enabled with a mapping to admin, and cur was
// off, did not map one of those groups to admin, or trusted another issuer, client
// or groups claim. (The default role can never be admin.)
func OIDCGrantsAdmin(cur, next OIDC) bool {
	groups := oidcAdminGroups(next)
	if !next.Enabled || len(groups) == 0 {
		return false
	}
	if !cur.Enabled || cur.Issuer != next.Issuer || cur.ClientID != next.ClientID || cur.GroupsClaim != next.GroupsClaim {
		return true
	}
	old := oidcAdminGroups(cur)
	for g := range groups {
		if !old[g] {
			return true
		}
	}
	return false
}

// oidcAdminGroups returns the groups o maps to admin.
func oidcAdminGroups(o OIDC) map[string]bool {
	out := map[string]bool{}
	for _, m := range o.RoleMappings {
		if strings.TrimSpace(m.Role) == "admin" {
			out[strings.TrimSpace(m.Group)] = true
		}
	}
	return out
}

// checkMetadataRetention refuses lowering metadata_backup.retention_count outside
// WithLoweredProtection: like a job's retention, it takes effect after the grace
// period.
func checkMetadataRetention(ctx context.Context, cur, next Settings) error {
	if next.MetadataBackup.RetentionCount < cur.MetadataBackup.RetentionCount && !loweringAllowed(ctx) {
		return fmt.Errorf("%w: metadata_backup.retention_count goes down from %d to %d", ErrProtectionLowered,
			cur.MetadataBackup.RetentionCount, next.MetadataBackup.RetentionCount)
	}
	return nil
}

// checkAdminGrant refuses, while the two-person rule is on, an oidc change that can
// grant admin (OIDCGrantsAdmin) outside WithLoweredProtection: the operations
// service asks a second administrator first.
func checkAdminGrant(ctx context.Context, cur, next Settings) error {
	if cur.Security.RequireSecondApprover && OIDCGrantsAdmin(cur.OIDC, next.OIDC) && !loweringAllowed(ctx) {
		return fmt.Errorf("%w: the oidc change can grant the admin role through single sign-on", ErrProtectionLowered)
	}
	return nil
}

// Preview returns cur with p applied and validated, without storing anything (the
// operations service uses it to tell what a change would do).
func Preview(cur Settings, p Patch) (Settings, error) {
	next, err := p.apply(cur, time.Now())
	if err != nil {
		return Settings{}, err
	}
	if err = validate(&next, false); err != nil {
		return Settings{}, err
	}
	return next, nil
}

// checkProtection refuses lowering a delete protection outside WithLoweredProtection.
func checkProtection(ctx context.Context, cur, next Security) error {
	grace, approver := LowersProtection(cur, next)
	if (grace || approver) && !loweringAllowed(ctx) {
		var errs []error
		if grace {
			errs = append(errs, fmt.Errorf("security.delete_grace_days goes down from %d to %d", cur.DeleteGraceDays, next.DeleteGraceDays))
		}
		if approver {
			errs = append(errs, errors.New("security.require_second_approver is turned off"))
		}
		return fmt.Errorf("%w: %w", ErrProtectionLowered, errors.Join(errs...))
	}
	return nil
}
