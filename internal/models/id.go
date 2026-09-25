package models

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// IDSuffixLength is the length of the random suffix NewIDSuffix returns.
const IDSuffixLength = 8

// NewIDSuffix returns IDSuffixLength random hexadecimal characters (crypto/rand) that
// make generated identifiers unique even when they are created within the same
// second for the same database.
func NewIDSuffix() (string, error) {
	b := make([]byte, IDSuffixLength/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// MaxIDLength is the maximum permitted length of a resource identifier.
const MaxIDLength = 64

// ErrInvalidID is returned when a client-supplied identifier does not match the
// permitted charset ^[a-zA-Z0-9_-]{1,64}$. Restricting identifiers keeps them safe
// to embed in URLs, storage keys, log lines, and HTML attributes.
var ErrInvalidID = errors.New("invalid id: must match ^[a-zA-Z0-9_-]{1,64}$")

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidateID reports whether id is a well-formed resource identifier.
// It returns ErrInvalidID when the identifier is empty, too long, or contains
// characters outside [a-zA-Z0-9_-].
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return ErrInvalidID
	}
	return nil
}

// SanitizeIDComponent converts an arbitrary string (such as a database name) into a
// fragment that is safe to embed in a generated identifier. Every character outside
// [a-zA-Z0-9_-] is replaced with '_' (letter case is preserved), and the result is
// truncated to at most maxLen bytes. A non-positive maxLen disables truncation.
func SanitizeIDComponent(s string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	out := b.String()
	if maxLen > 0 && len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}
