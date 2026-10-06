package auth

import (
	"bytes"
	"testing"
)

// TestImportedKeyMatchesOnlyMACs checks that an imported key is verified as an HMAC
// only, in both stored forms (with and without the MAC key ID), and that a plain
// SHA-256 digest of it never matches on that path (the dual check for legacy records
// lives in apiKeyMatches, for presented keys).
func TestImportedKeyMatchesOnlyMACs(t *testing.T) {
	const key = "correct-horse-battery-staple"
	macKey := bytes.Repeat([]byte{7}, 32)
	other := bytes.Repeat([]byte{8}, 32)
	s := &Service{importedKeyMACKey: macKey}

	withKID := importedKeyHash(macKey, key)
	withoutKID := importedKeyMACScheme + importedKeyMAC(macKey, key)
	for _, h := range []string{withKID, withoutKID} {
		if !s.importedKeyMatches(key, h, nil) || !s.importedKeyMatches(key, h, macKey) {
			t.Errorf("importedKeyMatches(%q) = false; want true", h)
		}
		if s.importedKeyMatches(key+"x", h, nil) {
			t.Errorf("a wrong key matches %q", h)
		}
		if !s.apiKeyMatches(key, h, nil) {
			t.Errorf("apiKeyMatches(%q) = false; want true", h)
		}
	}
	if s.importedKeyMatches(key, withKID, other) {
		t.Error("a hash naming another MAC key matched")
	}
	if s.importedKeyMatches(key, HashToken(key), nil) {
		t.Error("a plain SHA-256 digest matched on the imported-key path")
	}
	// Generated keys and legacy imported records keep their SHA-256 check.
	if !s.apiKeyMatches(key, HashToken(key), nil) {
		t.Error("apiKeyMatches no longer accepts a SHA-256 digest")
	}
}
