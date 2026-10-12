package models

// Fence is the fencing check of active-passive HA (implemented by *ha.Manager):
// runs check it before they start and before they are marked completed, and carry
// its epoch.
type Fence interface {
	// Check returns nil while the instance holds the HA lease.
	Check() error
	// Epoch returns the epoch the instance holds, or 0.
	Epoch() uint64
}
