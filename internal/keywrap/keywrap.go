// Package keywrap protects secret.key, the root key of the credentials stored in
// mongorescue.db (internal/secretbox), with an external key management service:
// AWS KMS, Google Cloud KMS, Azure Key Vault or the Transit engine of HashiCorp
// Vault (the providers live in internal/keywrap/provider).
//
// With MONGORESCUE_KEY_WRAP=<provider>:<key URI> set, the data directory holds
// secret.key.wrapped (a File: the provider, the key URI, the KMS key ID, the
// public fingerprint of the key and the ciphertext the KMS returned) and never the
// plaintext secret.key. Load unwraps it at startup with the instance's cloud
// identity, retrying a KMS that is briefly unreachable and failing with an
// actionable error when it stays so; it never falls back to a plaintext file.
// On the first start with wrapping on, Load wraps an existing plaintext secret.key
// (and the secret.key.next and secret.key.previous of a key rotation), writes the
// wrapped files atomically, checks that they unwrap to the same keys and only then
// overwrites and removes the plaintext files.
//
// Codec reads and writes wrapped key files for the key rotation
// (internal/keyrotation), so a rotated key is wrapped before it is installed.
// UnwrapToFiles and Rewrap are the offline operations of "mongorescue key": leaving
// the KMS, and moving to another KMS key. Changes of the wrapping are journaled in
// the data directory (see AppendJournal) and published as security events by the
// next start.
//
// Key material never reaches a log, an error or a metric label.
package keywrap

import (
	"context"
	"errors"
	"fmt"
)

// Wrapper wraps and unwraps a data key with one KMS key. Implementations are safe
// for concurrent use.
type Wrapper interface {
	// Provider returns the provider name (one of Providers).
	Provider() string
	// KeyID returns a stable identifier of the KMS key (not of a key version): the
	// key ARN, resource name or URL. It is recorded in secret.key.wrapped.
	KeyID() string
	// Wrap encrypts key with the KMS key and returns an opaque ciphertext.
	Wrap(ctx context.Context, key []byte) ([]byte, error)
	// Unwrap decrypts a ciphertext that Wrap returned (also with an older version
	// of the KMS key).
	Unwrap(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// Purpose is the context a wrapped key is bound to where the provider supports it
// (the AWS KMS encryption context, the additional authenticated data of Google
// Cloud KMS): a ciphertext of another application does not unwrap as secret.key.
const Purpose = "mongorescue/secret.key"

// Sentinel errors. Their messages are safe to show and to log.
var (
	// ErrUnavailable is returned when the KMS could not be reached or kept failing
	// for the whole retry budget.
	ErrUnavailable = errors.New("keywrap: the key management service is unreachable")
	// ErrUnwrap is returned when the KMS refused to unwrap the key: the ciphertext
	// was tampered with, belongs to another KMS key, or the identity lacks the
	// decrypt permission.
	ErrUnwrap = errors.New("keywrap: the key management service refused to unwrap secret.key")
	// ErrWrongKey is returned when an unwrapped key is not the key the wrapped file
	// records (its fingerprint differs).
	ErrWrongKey = errors.New("keywrap: the unwrapped key does not match the fingerprint recorded in the wrapped key file")
	// ErrMalformed is returned for a wrapped key file that is not valid.
	ErrMalformed = errors.New("keywrap: malformed wrapped key file")
	// ErrKeyMismatch is returned when the wrapped key file was wrapped with another
	// KMS key than the configured one.
	ErrKeyMismatch = errors.New("keywrap: secret.key.wrapped was wrapped with another KMS key than " + EnvKeyWrap + " names")
	// ErrConflict is returned when a plaintext key file and its wrapped counterpart
	// both exist and hold different keys.
	ErrConflict = errors.New("keywrap: a plaintext key file and its wrapped counterpart hold different keys")
	// ErrNotWrapped is returned by UnwrapToFiles and Rewrap for a data directory
	// without secret.key.wrapped.
	ErrNotWrapped = errors.New("keywrap: the data directory has no secret.key.wrapped")
	// ErrWrappedKeyNeedsConfig is returned at startup when secret.key.wrapped exists
	// but key wrapping is off.
	ErrWrappedKeyNeedsConfig = errors.New("keywrap: the data directory holds secret.key.wrapped but " + EnvKeyWrap +
		" is not set; set it to the provider and key URI recorded in the file, or run \"mongorescue key unwrap\" to leave the KMS")
)

// permanentError marks a KMS failure that a retry cannot fix (a refused unwrap,
// a missing permission or key).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks err as a failure that retrying does not fix; Load gives up on it
// at once. A nil err stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// unwrapError classifies an error of Wrapper.Unwrap for the caller: a permanent
// failure wraps ErrUnwrap, any other ErrUnavailable. The provider error is kept
// (it never carries key material).
func unwrapError(provider string, err error) error {
	if IsPermanent(err) {
		return fmt.Errorf("%w (%s): %w", ErrUnwrap, provider, err)
	}
	return fmt.Errorf("%w (%s): %w", ErrUnavailable, provider, err)
}
