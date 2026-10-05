package secretbox

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprintIsStableAndRevealsNothing(t *testing.T) {
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	fa, err := Fingerprint(a)
	if err != nil || len(fa) != 32 {
		t.Fatalf("Fingerprint = %q, %v", fa, err)
	}
	again, _ := Fingerprint(a)
	fb, _ := Fingerprint(b)
	if fa != again || fa == fb {
		t.Fatal("fingerprints must be deterministic and differ between keys")
	}
	if strings.Contains(EncodeKey(a), fa) {
		t.Fatal("fingerprint leaks the key")
	}
	if _, err = Fingerprint([]byte("short")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Fingerprint(short) = %v", err)
	}
}

func TestKeyFileHelpers(t *testing.T) {
	dir := t.TempDir()
	cur, next := filepath.Join(dir, KeyFileName), filepath.Join(dir, NextKeyFileName)
	if _, err := ReadKeyFile(cur); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadKeyFile(missing) = %v", err)
	}
	key, _ := GenerateKey()
	if err := WriteKeyFile(next, key); err != nil {
		t.Fatal(err)
	}
	if err := InstallKeyFile(next, cur); err != nil {
		t.Fatal(err)
	}
	got, err := ReadKeyFile(cur)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ReadKeyFile = %v", err)
	}
	if ok, _ := KeyFileExists(next); ok {
		t.Fatal("next key file still exists after install")
	}
	if err = RemoveKeyFile(next); err != nil {
		t.Fatalf("RemoveKeyFile(missing) = %v", err)
	}
	if err = WriteKeyFile(cur, []byte("short")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("WriteKeyFile(short) = %v", err)
	}
}

func TestRefFollowsTheStoredBox(t *testing.T) {
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	b1, _ := New(k1)
	b2, _ := New(k2)
	at := At("cookie", "flow", "value")
	ref := NewRef(b1)
	sealed, err := ref.Seal(at, "x")
	if err != nil {
		t.Fatal(err)
	}
	ref.Store(b2)
	if _, err = ref.Open(at, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open after Store = %v; want ErrDecrypt", err)
	}
	var empty *Ref
	if _, err = empty.Seal(at, "x"); err == nil {
		t.Fatal("Seal on a nil Ref must fail")
	}
}
