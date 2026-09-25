package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Password policy (bcrypt only uses the first 72 bytes, so longer input is rejected
// instead of being silently truncated).
const (
	MinPasswordLength = 12
	MaxPasswordBytes  = 72
	maxUsernameLength = 64
	maxKeyNameLength  = 100
)

// DefaultBcryptCost is the bcrypt work factor for password hashes.
const DefaultBcryptCost = 12

// API key layout: "mr_" + 8-character prefix + "_" + 32-character secret, all in
// lowercase base32 (a-z, 2-7).
const (
	apiKeyScheme    = "mr_"
	apiKeyPrefixLen = 8
	apiKeySecretLen = 32
)

// lowerBase32 encodes without padding in lowercase, which reads well and survives
// copy-paste and URL contexts.
var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// setupCodeGroups is the number of 4-character groups of a setup code (24 characters).
const setupCodeGroups = 6

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	switch {
	case utf8.RuneCountInString(pw) < MinPasswordLength:
		return fmt.Errorf("%w: must be at least %d characters", ErrInvalidPassword, MinPasswordLength)
	case len(pw) > MaxPasswordBytes:
		return fmt.Errorf("%w: must be at most %d bytes", ErrInvalidPassword, MaxPasswordBytes)
	case !utf8.ValidString(pw):
		return fmt.Errorf("%w: must be valid UTF-8", ErrInvalidPassword)
	}
	return nil
}

// ValidateUsername checks 1-64 characters of letters, digits and . _ @ -.
func ValidateUsername(name string) error {
	if name == "" || len(name) > maxUsernameLength {
		return fmt.Errorf("%w: must be 1-%d characters", ErrInvalidUsername, maxUsernameLength)
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '@' || r == '-'
		if !ok {
			return fmt.Errorf("%w: use letters, digits and . _ @ -", ErrInvalidUsername)
		}
	}
	return nil
}

// randomBytes returns n bytes from crypto/rand.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: random: %w", err)
	}
	return b, nil
}

// newToken returns a 32-byte random token in URL-safe base64.
func newToken() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newID returns prefix + 16 random hex characters.
func newID(prefix string) (string, error) {
	b, err := randomBytes(8)
	if err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a session token or API key, the only form
// that is persisted.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// equalHashes compares two hex digests in constant time.
func equalHashes(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// newAPIKey returns a new plaintext key and its public prefix.
func newAPIKey() (key, prefix string, err error) {
	b, err := randomBytes(25) // 5 bytes -> 8 chars prefix, 20 bytes -> 32 chars secret
	if err != nil {
		return "", "", err
	}
	prefix = lowerBase32.EncodeToString(b[:5])
	secret := lowerBase32.EncodeToString(b[5:])
	return apiKeyScheme + prefix + "_" + secret, prefix, nil
}

// parseAPIKey returns the prefix of a well-formed key.
func parseAPIKey(key string) (prefix string, ok bool) {
	rest, found := strings.CutPrefix(key, apiKeyScheme)
	if !found || len(rest) != apiKeyPrefixLen+1+apiKeySecretLen || rest[apiKeyPrefixLen] != '_' {
		return "", false
	}
	for i, r := range rest {
		if i == apiKeyPrefixLen {
			continue
		}
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return "", false
		}
	}
	return rest[:apiKeyPrefixLen], true
}

// newSetupCode returns a 24-character base32 code formatted as XXXX-XXXX-...
func newSetupCode() (string, error) {
	b, err := randomBytes(15) // 120 bits -> 24 base32 characters
	if err != nil {
		return "", err
	}
	raw := strings.ToUpper(lowerBase32.EncodeToString(b))
	groups := make([]string, 0, setupCodeGroups)
	for i := 0; i < len(raw); i += 4 {
		groups = append(groups, raw[i:i+4])
	}
	return strings.Join(groups, "-"), nil
}

// normalizeSetupCode strips separators and whitespace and upper-cases the code, so it
// may be typed with or without dashes.
func normalizeSetupCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if r != '-' && r != ' ' && r != '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
