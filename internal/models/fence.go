package models

import "context"

// Fence is the fencing check of active-passive HA (implemented by *ha.Manager):
// runs check it before they start and before they are marked completed, and carry
// its epoch.
type Fence interface {
	// Check returns nil while the instance holds the HA lease.
	Check() error
	// Epoch returns the epoch the instance holds, or 0.
	Epoch() uint64
}

// FenceRenewer is implemented by fences that can renew the lease on demand. The
// scheduler renews before it fires anything after a wall clock jump (a suspended
// machine), so the jump is seen by the lease before any run starts.
type FenceRenewer interface {
	// Renew renews the lease now (or steps down when that is no longer safe) and
	// returns the outcome of Check afterwards.
	Renew(ctx context.Context) error
}
