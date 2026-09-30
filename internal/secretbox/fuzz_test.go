package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

// fuzzKey is a fixed key so that fuzz inputs are reproducible.
var fuzzKey = bytes.Repeat([]byte{0x42}, KeySize)

// fuzzBox returns a Box with fuzzKey.
func fuzzBox(t testing.TB) *Box {
	b, err := New(fuzzKey)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FuzzOpen checks that Open and OpenLegacy never panic on arbitrary input and fail
// with ErrMalformed or ErrDecrypt (or ErrInvalidBinding) for values they cannot open.
func FuzzOpen(f *testing.F) {
	b := fuzzBox(f)
	sealed, err := b.Seal(here, "mongodb://u:p@h/db")
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"", "plaintext", Prefix, Prefix + "!!!", Prefix + "AAAA", LegacyPrefix + "AAAA", sealed, sealed + "\n", sealed[:len(sealed)-1]} {
		f.Add(s, "connections", "con_1", "uri")
	}
	f.Add(sealed, "", "con_1", "uri")
	f.Fuzz(func(t *testing.T, value, table, id, field string) {
		box := fuzzBox(t)
		at := At(table, id, field)
		plain, err := box.Open(at, value)
		if err == nil {
			// Only a value sealed with fuzzKey for this binding (a seed; worker
			// processes seal their own copy with another nonce) can open.
			if at != here || !IsSealed(value) || plain != "mongodb://u:p@h/db" {
				t.Fatalf("Open(%v, %q) = %q unexpectedly", at, value, plain)
			}
		} else if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrDecrypt) && !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("Open(%q): unexpected error %v", value, err)
		}
		if _, err := box.OpenLegacy(value); err != nil && !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrDecrypt) {
			t.Fatalf("OpenLegacy(%q): unexpected error %v", value, err)
		}
	})
}

// FuzzSealOpen checks that Seal then Open returns the plaintext and that changing any
// single byte of the sealed text makes Open fail.
func FuzzSealOpen(f *testing.F) {
	f.Add("mongodb://u:p@h/db", uint(0), byte(1))
	f.Add("", uint(10), byte(0x80))
	f.Add("sb2:AAAA", uint(1000), byte(0xff))
	f.Add("ünïcödé", uint(5), byte('\n'))
	f.Fuzz(func(t *testing.T, plain string, pos uint, xor byte) {
		box := fuzzBox(t)
		sealed, err := box.Seal(here, plain)
		if err != nil {
			if len(plain) > MaxPlaintextSize && errors.Is(err, ErrTooLarge) {
				return
			}
			t.Fatal(err)
		}
		got, err := box.Open(here, sealed)
		if err != nil || got != plain {
			t.Fatalf("Open(Seal(%q)) = %q, %v", plain, got, err)
		}
		if xor == 0 {
			return
		}
		mut := []byte(sealed)
		mut[pos%uint(len(mut))] ^= xor
		if got, err := box.Open(here, string(mut)); err == nil {
			t.Fatalf("tampered value %q (byte %d ^ %#x) opened as %q", mut, pos%uint(len(mut)), xor, got)
		}
		// Inserting a byte (such as a line break the decoder would skip) fails too.
		ins := sealed[:pos%uint(len(mut))] + string([]byte{xor}) + sealed[pos%uint(len(mut)):]
		if got, err := box.Open(here, ins); err == nil {
			t.Fatalf("value with inserted byte %#x at %d opened as %q", xor, pos%uint(len(mut)), got)
		}
	})
}
