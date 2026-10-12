package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Sentinel errors of conditional writes (see LeaseStore).
var (
	// ErrPreconditionFailed is returned by a conditional write whose condition does
	// not hold: CreateIfAbsent found the object, or ReplaceIfMatch found another
	// version or no object at all.
	ErrPreconditionFailed = errors.New("storage: precondition failed: the object exists or changed")

	// ErrConditionalWritesUnsupported is returned when a storage does not enforce
	// conditional writes: the driver has no LeaseStore capability, or the service
	// accepted a write whose condition did not hold (see ProbeLeaseStore).
	ErrConditionalWritesUnsupported = errors.New("storage: the storage does not enforce conditional writes")

	// ErrObjectTooLarge is returned by ReadVersioned for an object larger than
	// MaxLeaseObjectSize.
	ErrObjectTooLarge = errors.New("storage: the object is too large for a versioned read")
)

// MaxLeaseObjectSize bounds the objects LeaseStore reads and writes: they are small
// coordination documents (an HA lease), never archives.
const MaxLeaseObjectSize = 64 << 10

// LeaseStore is the optional capability of a driver that supports compare-and-swap
// writes of small objects, such as the lease of active-passive HA (internal/ha).
// Versions are opaque: an S3 ETag, or a content hash on local disk. Every write
// must have distinct content for its version to be distinct.
type LeaseStore interface {
	// ReadVersioned returns the content of key and its version. It returns
	// ErrNotFound when key does not exist and ErrObjectTooLarge above
	// MaxLeaseObjectSize.
	ReadVersioned(ctx context.Context, key string) (data []byte, version string, err error)

	// CreateIfAbsent stores data at key only if no object exists there, and returns
	// the new version. It returns ErrPreconditionFailed when the object exists.
	CreateIfAbsent(ctx context.Context, key string, data []byte) (version string, err error)

	// ReplaceIfMatch stores data at key only if the current version is version, and
	// returns the new version. It returns ErrPreconditionFailed when the object
	// has another version or does not exist.
	ReplaceIfMatch(ctx context.Context, key string, data []byte, version string) (newVersion string, err error)
}

// AsLeaseStore returns the LeaseStore capability of s, or an error wrapping
// ErrConditionalWritesUnsupported when its driver has none.
func AsLeaseStore(s Storage) (LeaseStore, error) {
	if ls, ok := s.(LeaseStore); ok {
		return ls, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrConditionalWritesUnsupported, s)
}

// ProbeLeaseStore checks that ls enforces its conditions, with a probe object at
// key (deleted again through del when it is not nil): a second CreateIfAbsent and a
// ReplaceIfMatch with a wrong version must both fail with ErrPreconditionFailed,
// and a ReplaceIfMatch with the right version must succeed. Some S3-compatible
// services accept the conditional headers and ignore them; the probe returns an
// error wrapping ErrConditionalWritesUnsupported for those.
func ProbeLeaseStore(ctx context.Context, ls LeaseStore, key string, del func(ctx context.Context, key string) error) (err error) {
	nonce := make([]byte, 8)
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("storage: probe: %w", err)
	}
	body := func(step string) []byte {
		return []byte(`{"probe":"` + hex.EncodeToString(nonce) + `","step":"` + step + `"}`)
	}
	version, err := ls.CreateIfAbsent(ctx, key, body("create"))
	if err != nil {
		return fmt.Errorf("storage: probe: create %s: %w", key, err)
	}
	if del != nil {
		defer func() {
			if delErr := del(context.WithoutCancel(ctx), key); delErr != nil && !errors.Is(delErr, ErrNotFound) && err == nil {
				err = fmt.Errorf("storage: probe: delete %s: %w", key, delErr)
			}
		}()
	}
	if _, err = ls.CreateIfAbsent(ctx, key, body("create-again")); !errors.Is(err, ErrPreconditionFailed) {
		if err == nil {
			return fmt.Errorf("%w: a create-if-absent (If-None-Match) of an existing object succeeded", ErrConditionalWritesUnsupported)
		}
		return fmt.Errorf("storage: probe: second create: %w", err)
	}
	if _, err = ls.ReplaceIfMatch(ctx, key, body("wrong-version"), wrongVersion(version)); !errors.Is(err, ErrPreconditionFailed) {
		if err == nil {
			return fmt.Errorf("%w: a replace with a wrong version (If-Match) succeeded", ErrConditionalWritesUnsupported)
		}
		return fmt.Errorf("storage: probe: replace with a wrong version: %w", err)
	}
	if _, err = ls.ReplaceIfMatch(ctx, key, body("replace"), version); err != nil {
		return fmt.Errorf("storage: probe: replace with the current version: %w", err)
	}
	data, _, err := ls.ReadVersioned(ctx, key)
	if err != nil {
		return fmt.Errorf("storage: probe: read back: %w", err)
	}
	if string(data) != string(body("replace")) {
		return fmt.Errorf("%w: the object does not hold the last successful write", ErrConditionalWritesUnsupported)
	}
	return nil
}

// wrongVersion returns a version that differs from v and has its shape (quoted for
// an S3 ETag).
func wrongVersion(v string) string {
	const fake = "00000000000000000000000000000000"
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if v == `"`+fake+`"` {
			return `"11111111111111111111111111111111"`
		}
		return `"` + fake + `"`
	}
	if v == fake {
		return "11111111111111111111111111111111"
	}
	return fake
}
