package encryption

import "testing"

func TestDecryptorHasMode(t *testing.T) {
	identity, _, err := GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	x, err := NewDecryptor(DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	if !x.HasMode(ModeX25519) || x.HasMode(ModeScrypt) {
		t.Fatalf("x25519 decryptor: HasMode(x25519)=%v HasMode(scrypt)=%v", x.HasMode(ModeX25519), x.HasMode(ModeScrypt))
	}
	p, err := NewDecryptor(DecryptorConfig{Passphrase: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if p.HasMode(ModeX25519) || !p.HasMode(ModeScrypt) {
		t.Fatalf("scrypt decryptor: HasMode(x25519)=%v HasMode(scrypt)=%v", p.HasMode(ModeX25519), p.HasMode(ModeScrypt))
	}
	var none *Decryptor
	if none.HasMode(ModeX25519) {
		t.Fatal("nil decryptor has a key")
	}
}
