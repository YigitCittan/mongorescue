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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
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

// leasePurpose derives the key of the lease MAC from secret.key.
const leasePurpose = "ha-lease"

// LeaseMACKey derives the key that authenticates the lease from secret.key (an
// HKDF subkey): every instance of the cluster shares it.
func LeaseMACKey(secretKey []byte) ([]byte, error) {
	key, err := secretbox.DeriveSubkey(secretKey, leasePurpose)
	if err != nil {
		return nil, fmt.Errorf("ha: derive the lease key: %w", err)
	}
	return key, nil
}

// SnapshotMark says how new a metadata snapshot is: its epoch, then its time.
type SnapshotMark struct {
	// Epoch is the lease epoch the snapshot was written under.
	Epoch uint64 `json:"epoch"`
	// At is when it was taken, to the millisecond.
	At time.Time `json:"at"`
}

// Before reports whether m is older than o.
func (m SnapshotMark) Before(o SnapshotMark) bool {
	if m.Epoch != o.Epoch {
		return m.Epoch < o.Epoch
	}
	return m.At.Before(o.At)
}

// IsZero reports whether m names no snapshot.
func (m SnapshotMark) IsZero() bool { return m.Epoch == 0 && m.At.IsZero() }

// String formats m for logs and errors.
func (m SnapshotMark) String() string {
	if m.IsZero() {
		return "none"
	}
	return fmt.Sprintf("epoch %d at %s", m.Epoch, m.At.UTC().Format(time.RFC3339Nano))
}

// LaterMark returns the newer of a and b.
func LaterMark(a, b SnapshotMark) SnapshotMark {
	if a.Before(b) {
		return b
	}
	return a
}

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
	// LastSnapshotEpoch and LastSnapshotAt name the newest metadata snapshot the
	// cluster's active instances wrote or restored: a takeover must find a
	// verified snapshot at least as new, or someone deleted the newer ones.
	LastSnapshotEpoch uint64    `json:"last_snapshot_epoch,omitempty"`
	LastSnapshotAt    time.Time `json:"last_snapshot_at,omitzero"`
	// MAC authenticates the cluster, epoch, holder and last snapshot of the lease
	// (HMAC-SHA256, hex, with the key of LeaseMACKey). It is empty in a lease of an
	// earlier version, and in the lease of an instance whose takeover is not
	// complete: such a lease is never trusted.
	MAC string `json:"mac,omitempty"`
}

// SnapshotMark returns the last snapshot the lease names.
func (l Lease) SnapshotMark() SnapshotMark {
	return SnapshotMark{Epoch: l.LastSnapshotEpoch, At: l.LastSnapshotAt}
}

// withSnapshotMark returns l naming m as its last snapshot.
func (l Lease) withSnapshotMark(m SnapshotMark) Lease {
	l.LastSnapshotEpoch, l.LastSnapshotAt = m.Epoch, m.At.UTC()
	if m.At.IsZero() {
		l.LastSnapshotAt = time.Time{}
	}
	return l
}

// macInput is the authenticated content of l.
func (l Lease) macInput() []byte {
	at := ""
	if !l.LastSnapshotAt.IsZero() {
		at = l.LastSnapshotAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Appendf(nil, "mongorescue-ha-lease/1\x00%s\x00%d\x00%s\x00%d\x00%s", l.Cluster, l.Epoch, l.Holder, l.LastSnapshotEpoch, at)
}

// leaseMAC returns the MAC of l under key.
func leaseMAC(l Lease, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(l.macInput())
	return h.Sum(nil)
}

// sign returns l with its MAC under key.
func (l Lease) sign(key []byte) Lease {
	l.MAC = hex.EncodeToString(leaseMAC(l, key))
	return l
}

// Authentic reports whether l carries a valid MAC under key.
func (l Lease) Authentic(key []byte) bool {
	mac, err := hex.DecodeString(l.MAC)
	return err == nil && l.MAC != "" && hmac.Equal(mac, leaseMAC(l, key))
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
