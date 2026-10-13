package keywrap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Suffix is appended to the name of a key file for its wrapped counterpart:
// secret.key.wrapped, secret.key.next.wrapped and secret.key.previous.wrapped.
const Suffix = ".wrapped"

// WrappedKeyFileName is the wrapped secret.key in the data directory.
const WrappedKeyFileName = secretbox.KeyFileName + Suffix

// FileFormat identifies a wrapped key file; FileVersion is its version.
const (
	FileFormat  = "mongorescue-wrapped-key"
	FileVersion = 1
)

// maxFileSize bounds a wrapped key file (a KMS ciphertext of a 32-byte key is a
// few hundred bytes; an RSA-4096 one about 700).
const maxFileSize = 64 << 10

// File is the content of a wrapped key file (JSON). It holds no secret: the
// ciphertext only opens with the KMS key, and the fingerprint is a one-way
// identifier (secretbox.Fingerprint).
type File struct {
	// Format is FileFormat and Version FileVersion.
	Format  string `json:"format"`
	Version int    `json:"version"`
	// Provider and KeyURI are the Spec the key was wrapped with.
	Provider string `json:"provider"`
	KeyURI   string `json:"key_uri"`
	// KeyID is the stable identifier of the KMS key (Wrapper.KeyID).
	KeyID string `json:"key_id"`
	// Fingerprint is secretbox.Fingerprint of the wrapped key: Open checks it, and
	// it tells instances apart that share a key without unwrapping it.
	Fingerprint string `json:"fingerprint"`
	// Ciphertext is what Wrapper.Wrap returned (base64 in JSON).
	Ciphertext []byte `json:"ciphertext"`
	// WrappedAt is when the key was wrapped (UTC).
	WrappedAt time.Time `json:"wrapped_at"`
}

// Spec returns the provider and key URI the file was wrapped with.
func (f *File) Spec() Spec { return Spec{Provider: f.Provider, KeyURI: f.KeyURI} }

// Seal wraps key with w and returns the wrapped key file content for spec.
func Seal(ctx context.Context, w Wrapper, spec Spec, key []byte, now time.Time) (*File, error) {
	if len(key) != secretbox.KeySize {
		return nil, secretbox.ErrInvalidKey
	}
	fp, err := secretbox.Fingerprint(key)
	if err != nil {
		return nil, err
	}
	ct, err := w.Wrap(ctx, key)
	if err != nil {
		if IsPermanent(err) {
			return nil, fmt.Errorf("keywrap: the key management service refused to wrap the key (%s): %w", w.Provider(), err)
		}
		return nil, fmt.Errorf("%w (%s): wrap: %w", ErrUnavailable, w.Provider(), err)
	}
	if len(ct) == 0 {
		return nil, fmt.Errorf("%w: %s returned an empty ciphertext", ErrUnavailable, w.Provider())
	}
	return &File{Format: FileFormat, Version: FileVersion, Provider: spec.Provider, KeyURI: spec.KeyURI,
		KeyID: w.KeyID(), Fingerprint: fp, Ciphertext: ct, WrappedAt: now.UTC()}, nil
}

// Open unwraps the key of f with w and checks it against the recorded fingerprint.
func (f *File) Open(ctx context.Context, w Wrapper) ([]byte, error) {
	key, err := w.Unwrap(ctx, f.Ciphertext)
	if err != nil {
		return nil, unwrapError(w.Provider(), err)
	}
	if len(key) != secretbox.KeySize {
		return nil, ErrWrongKey
	}
	fp, err := secretbox.Fingerprint(key)
	if err != nil {
		return nil, err
	}
	if fp != f.Fingerprint {
		return nil, ErrWrongKey
	}
	return key, nil
}

// validate checks the structure of f.
func (f *File) validate() error {
	switch {
	case f.Format != FileFormat:
		return fmt.Errorf("%w: unknown format", ErrMalformed)
	case f.Version != FileVersion:
		return fmt.Errorf("%w: unsupported version %d", ErrMalformed, f.Version)
	case len(f.Ciphertext) == 0 || f.Fingerprint == "":
		return fmt.Errorf("%w: no ciphertext or fingerprint", ErrMalformed)
	}
	if _, err := ParseSpec(f.Provider + ":" + f.KeyURI); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return nil
}

// ReadFile reads and validates the wrapped key file at path. A missing file is an
// error wrapping fs.ErrNotExist.
func ReadFile(path string) (*File, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a key file in the configured data directory.
	if err != nil {
		return nil, fmt.Errorf("keywrap: read %s: %w", filepath.Base(path), err)
	}
	return ParseFile(data)
}

// ParseFile parses and validates the content of a wrapped key file.
func ParseFile(data []byte) (*File, error) {
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("%w: too large", ErrMalformed)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Marshal returns the file content of f.
func (f *File) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("keywrap: encode wrapped key: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteFile writes f to path atomically (temporary file, fsync, rename, sync of
// the directory) with mode 0600.
func WriteFile(path string, f *File) error {
	data, err := f.Marshal()
	if err != nil {
		return err
	}
	return secretbox.WriteFileAtomic(path, data)
}
