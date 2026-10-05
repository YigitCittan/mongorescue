package settings

import (
	"context"
	"errors"
	"fmt"
)

// ErrProtectionLowered is returned by Update for a change that lowers a delete
// protection directly: security.delete_grace_days lowered, or
// security.require_second_approver turned off. Those changes go through the
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
