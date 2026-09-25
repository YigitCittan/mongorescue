// Package secretbox encrypts credentials at rest (MongoDB connection strings,
// notification channel secrets, storage credentials and encryption keys) with
// AES-256-GCM.
//
// A sealed value is the text "sb2:" followed by the standard base64 encoding of
// version byte || 12-byte random nonce || ciphertext+tag. The associated data binds
// every value to its location (a Binding: table, record ID and field), so a
// ciphertext copied into another record or field no longer opens. The textual prefix
// lets callers tell sealed values from plaintext; the version byte allows the format
// to evolve without ambiguity.
//
// The earlier "sb1:" format authenticated only the version byte. It is still opened
// by OpenLegacy so that a one-time migration can re-seal old values as "sb2:".
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

// Prefix marks a value sealed in the current format.
const Prefix = "sb2:"

// LegacyPrefix marks a value sealed in the original format, which did not bind the
// value to its location. Such values are only opened by OpenLegacy.
const LegacyPrefix = "sb1:"

// Format version bytes (the first byte of every sealed payload).
const (
	formatVersion       byte = 2
	legacyFormatVersion byte = 1
)

// Sentinel errors.
var (
	// ErrInvalidKey is returned when a key is not KeySize bytes (after base64 decoding).
	ErrInvalidKey = errors.New("secretbox: key must be 32 bytes (base64-encoded)")
	// ErrMalformed is returned when a value is not a well-formed sealed value, for
	// example plaintext or a value in the wrong format.
	ErrMalformed = errors.New("secretbox: malformed sealed value")
	// ErrInvalidBinding is returned for a Binding with an empty component.
	ErrInvalidBinding = errors.New("secretbox: binding needs a table, record ID and field")
	// ErrDecrypt is returned when authentication fails: the value was tampered with or
	// sealed with a different key.
	ErrDecrypt = errors.New("secretbox: decryption failed (wrong key or tampered value)")
	// ErrSecretKeyMismatch is returned at startup when the database holds values
	// encrypted with a key other than the configured one (or the key is missing).
	ErrSecretKeyMismatch = errors.New("secretbox: the secret key does not match the key this database was encrypted with")
)

// Box seals and opens values with one AES-256-GCM key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a KeySize-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// GenerateKey returns a new random key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secretbox: generate key: %w", err)
	}
	return key, nil
}

// ParseKey decodes a base64 (standard or URL alphabet, padded or not) key.
func ParseKey(encoded string) ([]byte, error) {
	s := strings.TrimSpace(encoded)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != KeySize {
				return nil, ErrInvalidKey
			}
			return key, nil
		}
	}
	return nil, ErrInvalidKey
}

// EncodeKey returns the canonical (standard base64) text form of key.
func EncodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// Binding names the location of a sealed value. It is authenticated as associated
// data, so a value only opens at the location it was sealed for. Record IDs must
// therefore be immutable.
type Binding struct {
	// Table is the table (or other namespace) holding the value.
	Table string
	// RecordID identifies the record within Table.
	RecordID string
	// Field names the field within the record, e.g. "uri" or "webhook.headers.X-Token".
	Field string
}

// At returns a Binding for table, id and field.
func At(table, id, field string) Binding {
	return Binding{Table: table, RecordID: id, Field: field}
}

// String returns the "<table>|<record id>|<field>" form used in error messages.
func (b Binding) String() string {
	return b.Table + "|" + b.RecordID + "|" + b.Field
}

// aad encodes version || len-prefixed table, record ID and field, which is
// unambiguous whatever characters the components contain.
func (b Binding) aad(version byte) ([]byte, error) {
	if b.Table == "" || b.RecordID == "" || b.Field == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidBinding, b.String())
	}
	out := make([]byte, 0, 1+3*binary.MaxVarintLen64+len(b.Table)+len(b.RecordID)+len(b.Field))
	out = append(out, version)
	for _, part := range []string{b.Table, b.RecordID, b.Field} {
		out = binary.AppendUvarint(out, uint64(len(part)))
		out = append(out, part...)
	}
	return out, nil
}

// IsSealed reports whether s carries the current sealed-value prefix.
func IsSealed(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// IsLegacySealed reports whether s carries the legacy "sb1:" prefix.
func IsLegacySealed(s string) bool {
	return strings.HasPrefix(s, LegacyPrefix)
}

// Seal encrypts plaintext for the location at, with a fresh random nonce. Every value
// is sealed, including one that happens to look like a sealed value already.
func (b *Box) Seal(at Binding, plaintext string) (string, error) {
	aad, err := at.aad(formatVersion)
	if err != nil {
		return "", err
	}
	nonceSize := b.aead.NonceSize()
	buf := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+b.aead.Overhead())
	buf[0] = formatVersion
	if _, err := rand.Read(buf[1:]); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	out := b.aead.Seal(buf, buf[1:1+nonceSize], []byte(plaintext), aad)
	return Prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a value that Seal produced for the same location. It returns
// ErrMalformed for plaintext and legacy values and ErrDecrypt when authentication
// fails (wrong key, tampered value, or a value sealed for another location).
func (b *Box) Open(at Binding, sealed string) (string, error) {
	aad, err := at.aad(formatVersion)
	if err != nil {
		return "", err
	}
	if !IsSealed(sealed) {
		return "", ErrMalformed
	}
	return b.open(sealed[len(Prefix):], formatVersion, aad)
}

// OpenLegacy decrypts a value in the legacy "sb1:" format, whose associated data is
// only the version byte. It exists for the one-time migration to the current format.
func (b *Box) OpenLegacy(sealed string) (string, error) {
	if !IsLegacySealed(sealed) {
		return "", ErrMalformed
	}
	return b.open(sealed[len(LegacyPrefix):], legacyFormatVersion, []byte{legacyFormatVersion})
}

// open decodes and authenticates a base64 payload of the given version.
func (b *Box) open(payload string, version byte, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	nonceSize := b.aead.NonceSize()
	if err != nil || len(raw) < 1+nonceSize+b.aead.Overhead() {
		return "", ErrMalformed
	}
	if raw[0] != version {
		return "", fmt.Errorf("%w: unsupported version %d", ErrMalformed, raw[0])
	}
	plain, err := b.aead.Open(nil, raw[1:1+nonceSize], raw[1+nonceSize:], aad)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}
