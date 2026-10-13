// Package keywraptest provides a fake keywrap.Wrapper for hermetic tests: an
// in-memory AES-256-GCM "KMS key" that can be made unreachable or refusing.
package keywraptest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

// ErrDown is returned by a Fake that is down (a KMS that cannot be reached).
var ErrDown = errors.New("keywraptest: the fake KMS is unreachable")

// ErrRefused is returned (marked permanent) for a ciphertext the fake cannot open.
var ErrRefused = errors.New("keywraptest: the fake KMS refused the ciphertext")

// Fake is an in-memory keywrap.Wrapper. Fakes with the same Seed open each other's
// ciphertexts (the same KMS key seen by two instances). It is safe for concurrent
// use.
type Fake struct {
	// Name is the provider name (default keywrap.ProviderAWSKMS); ID the key ID.
	Name, ID string

	aead cipher.AEAD
	mu   sync.Mutex
	// down makes the next calls fail with ErrDown (negative: for ever).
	down int
	// Wraps and Unwraps count the calls.
	Wraps, Unwraps atomic.Int64
}

// New returns a Fake keyed by seed (any length; padded or cut to 32 bytes).
func New(seed string) *Fake {
	k := make([]byte, 32)
	copy(k, seed)
	block, err := aes.NewCipher(k)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Fake{Name: keywrap.ProviderAWSKMS, ID: "fake-key-" + seed, aead: aead}
}

// SetDown makes the next n calls fail with ErrDown; n < 0 for every call, 0 to
// recover.
func (f *Fake) SetDown(n int) {
	f.mu.Lock()
	f.down = n
	f.mu.Unlock()
}

// fail reports whether this call fails as unreachable.
func (f *Fake) fail() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.down < 0:
		return true
	case f.down > 0:
		f.down--
		return true
	}
	return false
}

// Provider returns Name.
func (f *Fake) Provider() string { return f.Name }

// KeyID returns ID.
func (f *Fake) KeyID() string { return f.ID }

// Wrap seals key.
func (f *Fake) Wrap(ctx context.Context, key []byte) ([]byte, error) {
	f.Wraps.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.fail() {
		return nil, ErrDown
	}
	nonce := make([]byte, f.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return f.aead.Seal(nonce, nonce, key, []byte(keywrap.Purpose)), nil
}

// Unwrap opens a ciphertext of Wrap.
func (f *Fake) Unwrap(ctx context.Context, ct []byte) ([]byte, error) {
	f.Unwraps.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.fail() {
		return nil, ErrDown
	}
	n := f.aead.NonceSize()
	if len(ct) < n {
		return nil, keywrap.Permanent(ErrRefused)
	}
	key, err := f.aead.Open(nil, ct[:n], ct[n:], []byte(keywrap.Purpose))
	if err != nil {
		return nil, keywrap.Permanent(fmt.Errorf("%w: %w", ErrRefused, err))
	}
	return key, nil
}

// Opener returns a keywrap.OpenFunc that maps key URIs to fakes (any other spec
// is an error).
func Opener(fakes map[string]*Fake) keywrap.OpenFunc {
	return func(_ context.Context, spec keywrap.Spec) (keywrap.Wrapper, error) {
		if f, ok := fakes[spec.KeyURI]; ok {
			return f, nil
		}
		return nil, fmt.Errorf("keywraptest: no fake for %s", spec)
	}
}

// NoSleep is a keywrap retry sleep that returns at once.
func NoSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }
