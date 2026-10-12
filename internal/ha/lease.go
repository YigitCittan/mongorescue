// Package ha implements active-passive high availability (docs/design/ha.md): a
// lease on a storage target decides which of the instances of a cluster is active
// and runs scheduled backups, PITR collection and the other background work; the
// others wait as standbys and take over when the lease expires.
//
// The lease is a small JSON object written with compare-and-swap
// (storage.LeaseStore). Its epoch is a fencing token that grows with every
// takeover. No decision reads another instance's clock: every instance measures
// expiry with its own monotonic clock from the moment it observed a change.
//
// The package is a business package: it depends on the storage port, never on the
// HTTP server.
package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Role is the role of an instance in a cluster.
type Role string

const (
	// RoleStandby waits for the lease and runs no background work.
	RoleStandby Role = "standby"
	// RoleActive holds the lease and runs the background work.
	RoleActive Role = "active"
)

// Sentinel errors.
var (
	// ErrNotActive is returned by Manager.Check when the instance does not hold
	// the lease (or can no longer be sure it does): runs must not start or complete.
	ErrNotActive = errors.New("ha: this instance does not hold the HA lease")

	// ErrKeyMismatch reports a lease written by an instance with another secret key:
	// this instance could not read the active instance's snapshots or credentials.
	ErrKeyMismatch = errors.New("ha: the HA lease was written by an instance with a different secret key; give every instance of the cluster the same MONGORESCUE_SECRET_KEY")

	// ErrInvalidConfig reports a manager configuration out of range.
	ErrInvalidConfig = errors.New("ha: invalid configuration")
)

// Prefix is the storage key prefix of the HA objects of every cluster.
const Prefix = "_mongorescue/ha/"

// clusterPattern is the shape of a cluster name.
var clusterPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateCluster checks a cluster name: 1 to 63 lowercase letters, digits, '.',
// '_' and '-', starting with a letter or digit.
func ValidateCluster(name string) error {
	if !clusterPattern.MatchString(name) {
		return fmt.Errorf("%w: cluster name %q must be 1-63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidConfig, name)
	}
	return nil
}

// LeaseKey returns the storage key of the lease of cluster.
func LeaseKey(cluster string) string { return Prefix + cluster + "/lease.json" }

// ProbeKey returns the storage key of the conditional-write probe of an instance.
func ProbeKey(cluster, instanceID string) string {
	return Prefix + cluster + "/probe-" + instanceID + ".json"
}

// Lease is the content of the lease object.
type Lease struct {
	// Cluster is the cluster name.
	Cluster string `json:"cluster"`
	// Holder is the instance ID of the active instance; empty once released.
	Holder string `json:"holder"`
	// HolderName is a label of the holder (its host name), for people.
	HolderName string `json:"holder_name,omitempty"`
	// Epoch is the fencing token: it grows by one with every takeover.
	Epoch uint64 `json:"epoch"`
	// Seq grows with every write, so every write has distinct content.
	Seq uint64 `json:"seq"`
	// RenewedAt is the holder's wall clock at the write, for people only: no
	// decision reads it.
	RenewedAt time.Time `json:"renewed_at"`
	// TTLSeconds is the holder's lease TTL.
	TTLSeconds int64 `json:"ttl_seconds"`
	// KeyID identifies the secret key of the holder (an HKDF subkey of it).
	KeyID string `json:"key_id,omitempty"`
}

// Released reports whether the lease has no holder.
func (l Lease) Released() bool { return l.Holder == "" }

// ttl returns the holder's TTL.
func (l Lease) ttl() time.Duration { return time.Duration(l.TTLSeconds) * time.Second }

// encode returns the JSON of l.
func (l Lease) encode() ([]byte, error) {
	data, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("ha: encode lease: %w", err)
	}
	return data, nil
}

// DecodeLease parses the content of a lease object.
func DecodeLease(data []byte) (Lease, error) {
	var l Lease
	if err := json.Unmarshal(data, &l); err != nil {
		return Lease{}, fmt.Errorf("ha: decode lease: %w", err)
	}
	return l, nil
}
