package secretbox

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"testing"
)

// TestDeriveSubkey checks that subkeys are HKDF-SHA256 outputs bound to their
// purpose: deterministic, independent of each other and never the master key.
func TestDeriveSubkey(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, KeySize)
	a, err := DeriveSubkey(master, "auth/imported-api-key")
	if err != nil || len(a) != KeySize {
		t.Fatalf("DeriveSubkey = %x, %v", a, err)
	}
	again, _ := DeriveSubkey(master, "auth/imported-api-key")
	other, _ := DeriveSubkey(master, "something-else")
	if !bytes.Equal(a, again) {
		t.Error("the same purpose must give the same subkey")
	}
	if bytes.Equal(a, other) || bytes.Equal(a, master) || bytes.Equal(other, master) {
		t.Error("subkeys must differ per purpose and from the master key")
	}
	want, err := hkdf.Key(sha256.New, master, nil, SubkeyInfoPrefix+"auth/imported-api-key", KeySize)
	if err != nil || !bytes.Equal(a, want) {
		t.Errorf("subkey = %x; want HKDF-SHA256 output %x", a, want)
	}
	otherMaster, _ := DeriveSubkey(bytes.Repeat([]byte{0x43}, KeySize), "auth/imported-api-key")
	if bytes.Equal(a, otherMaster) {
		t.Error("another master key must give another subkey")
	}
	if _, err := DeriveSubkey(master[:16], "x"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("short master: %v; want ErrInvalidKey", err)
	}
	if _, err := DeriveSubkey(master, ""); err == nil {
		t.Error("an empty purpose must be refused")
	}
}
