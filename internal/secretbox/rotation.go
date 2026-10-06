package secretbox

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Key files of a secret key rotation, next to KeyFileName in the data directory.
const (
	// NextKeyFileName holds the new key while a rotation is in progress: it is written
	// (and synced) before the database is re-sealed, so a crash after the commit can
	// still install it.
	NextKeyFileName = KeyFileName + ".next"
	// PreviousKeyFileName keeps the key a rotation replaced, so that metadata
	// snapshots sealed with it stay readable until the next recovery kit is taken.
	PreviousKeyFileName = KeyFileName + ".previous"
)

// fingerprintPurpose derives the public fingerprint of a key (see Fingerprint).
const fingerprintPurpose = "key-fingerprint"

// Fingerprint returns a public identifier of key: 32 hex characters of an HKDF
// subkey. It tells keys apart in logs, events and the rotation marker without
// revealing anything about the key itself.
func Fingerprint(key []byte) (string, error) {
	sub, err := DeriveSubkey(key, fingerprintPurpose)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sub[:16]), nil
}

// ReadKeyFile reads and parses the key file at path. A missing file is an error
// wrapping fs.ErrNotExist.
func ReadKeyFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path) //nolint:gosec // G304: path is a key file in the configured data directory.
	if err != nil {
		return nil, fmt.Errorf("secretbox: read key file: %w", err)
	}
	key, err := ParseKey(string(content))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return key, nil
}

// WriteKeyFile writes key to path atomically (temporary file, fsync, rename, sync of
// the directory) with mode 0600, replacing an existing file.
func WriteKeyFile(path string, key []byte) error {
	if len(key) != KeySize {
		return ErrInvalidKey
	}
	return writeFileAtomic(path, []byte(EncodeKey(key)+"\n"))
}

// InstallKeyFile renames from to to (replacing it) and syncs the directory, so the
// new key file survives a power loss once the call returns.
func InstallKeyFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("secretbox: install key file: %w", err)
	}
	if err := syncDir(filepath.Dir(to)); err != nil {
		return fmt.Errorf("secretbox: sync key directory: %w", err)
	}
	return nil
}

// RemoveKeyFile removes the key file at path; a missing file is not an error.
func RemoveKeyFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secretbox: remove key file: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// tempKeyPattern matches the temporary files writeFileAtomic creates.
const tempKeyPattern = ".secret.key.tmp-*"

// RemoveStaleTempFiles removes temporary key files a crash left in dir (they are
// renamed into place on success, so any that remain belong to an interrupted write)
// and returns how many it removed. Only call it while no key file is being written.
func RemoveStaleTempFiles(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, tempKeyPattern))
	if err != nil {
		return 0, fmt.Errorf("secretbox: find temporary key files: %w", err)
	}
	removed := 0
	var errs []error
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("secretbox: remove temporary key files: %w", errors.Join(errs...))
	}
	return removed, nil
}

// KeyFileExists reports whether a key file exists at path.
func KeyFileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("secretbox: stat key file: %w", err)
	}
}

// Ref holds a Box that a secret key rotation replaces while the application runs.
// It is safe for concurrent use.
type Ref struct {
	box atomic.Pointer[Box]
}

// NewRef returns a Ref holding box.
func NewRef(box *Box) *Ref {
	r := &Ref{}
	r.box.Store(box)
	return r
}

// Box returns the current Box (nil when none was set).
func (r *Ref) Box() *Box {
	if r == nil {
		return nil
	}
	return r.box.Load()
}

// Store replaces the Box.
func (r *Ref) Store(box *Box) { r.box.Store(box) }

// Seal seals with the current Box.
func (r *Ref) Seal(at Binding, plaintext string) (string, error) {
	b := r.Box()
	if b == nil {
		return "", ErrInvalidKey
	}
	return b.Seal(at, plaintext)
}

// Open opens with the current Box.
func (r *Ref) Open(at Binding, sealed string) (string, error) {
	b := r.Box()
	if b == nil {
		return "", ErrInvalidKey
	}
	return b.Open(at, sealed)
}
