package settings

import (
	"bytes"
	"io"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
)

func encrypt(t *testing.T, enc *encryption.Encryptor, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decrypt(t *testing.T, dec *encryption.Decryptor, ciphertext []byte, want string) {
	t.Helper()
	if dec == nil {
		t.Fatal("no decryptor")
	}
	r, err := dec.Decrypt(bytes.NewReader(ciphertext))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != want {
		t.Fatalf("decrypt = %q, %v; want %q", got, err, want)
	}
}
