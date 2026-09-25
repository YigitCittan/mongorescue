package encryption

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

// testWorkFactor keeps scrypt fast in tests; production uses DefaultScryptWorkFactor.
const testWorkFactor = 10

func genKey(t *testing.T) (identity, recipient string) {
	t.Helper()
	id, rcpt, err := GenerateX25519()
	if err != nil {
		t.Fatalf("GenerateX25519: %v", err)
	}
	return id, rcpt
}

func encrypt(t *testing.T, enc *Encryptor, plaintext []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func decrypt(dec *Decryptor, ciphertext []byte) ([]byte, error) {
	r, err := dec.Decrypt(bytes.NewReader(ciphertext))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestX25519RoundTripMultiRecipient(t *testing.T) {
	id1, r1 := genKey(t)
	id2, r2 := genKey(t)

	enc, err := NewX25519Encryptor([]string{r1, " ", r2})
	if err != nil {
		t.Fatalf("NewX25519Encryptor: %v", err)
	}
	if enc.Mode() != ModeX25519 {
		t.Fatalf("mode = %q, want %q", enc.Mode(), ModeX25519)
	}

	payload := randomPayload(t, 3*64*1024+17) // spans several age chunks
	ciphertext := encrypt(t, enc, payload)
	if bytes.Contains(ciphertext, payload[:64]) {
		t.Fatal("ciphertext contains plaintext")
	}

	for i, id := range []string{id1, id2} {
		dec, err := NewDecryptor(DecryptorConfig{Identity: id})
		if err != nil {
			t.Fatalf("NewDecryptor #%d: %v", i, err)
		}
		got, err := decrypt(dec, ciphertext)
		if err != nil {
			t.Fatalf("decrypt with identity #%d: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("identity #%d: plaintext mismatch", i)
		}
	}
}

func TestScryptRoundTrip(t *testing.T) {
	enc, err := NewScryptEncryptor("correct horse battery staple", testWorkFactor)
	if err != nil {
		t.Fatalf("NewScryptEncryptor: %v", err)
	}
	if enc.Mode() != ModeScrypt {
		t.Fatalf("mode = %q, want %q", enc.Mode(), ModeScrypt)
	}
	payload := []byte("mongodump archive bytes")
	ciphertext := encrypt(t, enc, payload)

	dec, err := NewDecryptor(DecryptorConfig{Passphrase: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	got, err := decrypt(dec, ciphertext)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}

	wrong, _ := NewDecryptor(DecryptorConfig{Passphrase: "wrong passphrase"})
	if _, err := decrypt(wrong, ciphertext); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("wrong passphrase: expected ErrDecryptionFailed, got %v", err)
	}
}

func TestDecryptWrongIdentity(t *testing.T) {
	_, r1 := genKey(t)
	other, _ := genKey(t)
	enc, _ := NewX25519Encryptor([]string{r1})
	ciphertext := encrypt(t, enc, []byte("secret data"))

	dec, _ := NewDecryptor(DecryptorConfig{Identity: other})
	_, err := decrypt(dec, ciphertext)
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got %v", err)
	}
	var noMatch *age.NoIdentityMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("expected wrapped *age.NoIdentityMatchError, got %T", errors.Unwrap(err))
	}
	if strings.Contains(err.Error(), "AGE-SECRET-KEY") {
		t.Fatal("error message leaks identity material")
	}
}

func TestDecryptTamperedStream(t *testing.T) {
	id, r := genKey(t)
	enc, _ := NewX25519Encryptor([]string{r})
	ciphertext := encrypt(t, enc, randomPayload(t, 200*1024))
	dec, _ := NewDecryptor(DecryptorConfig{Identity: id})

	tampered := bytes.Clone(ciphertext)
	tampered[len(tampered)-10] ^= 0xFF
	if _, err := decrypt(dec, tampered); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("tampered: expected ErrDecryptionFailed, got %v", err)
	}

	truncated := ciphertext[:len(ciphertext)-1000]
	if _, err := decrypt(dec, truncated); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("truncated: expected ErrDecryptionFailed, got %v", err)
	}
}

type failingReader struct {
	r   io.Reader
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

func TestDecryptSourceErrorIsNotDecryptionFailure(t *testing.T) {
	id, r := genKey(t)
	enc, _ := NewX25519Encryptor([]string{r})
	ciphertext := encrypt(t, enc, randomPayload(t, 300*1024))
	dec, _ := NewDecryptor(DecryptorConfig{Identity: id})

	ioErr := errors.New("s3: connection reset")
	plain, err := dec.Decrypt(&failingReader{r: bytes.NewReader(ciphertext), n: 100 * 1024, err: ioErr})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	_, err = io.ReadAll(plain)
	if !errors.Is(err, ioErr) || errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected pass-through I/O error, got %v", err)
	}
}

func TestInvalidRecipientDoesNotEchoValue(t *testing.T) {
	secret, r := genKey(t)
	_, err := NewX25519Encryptor([]string{r, secret})
	if !errors.Is(err, ErrInvalidRecipient) {
		t.Fatalf("expected ErrInvalidRecipient, got %v", err)
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "#1") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestKeyRequired(t *testing.T) {
	if _, err := NewX25519Encryptor([]string{"", "  "}); !errors.Is(err, ErrEncryptionKeyRequired) {
		t.Fatalf("empty recipients: got %v", err)
	}
	if _, err := NewScryptEncryptor("", 0); !errors.Is(err, ErrEncryptionKeyRequired) {
		t.Fatalf("empty passphrase: got %v", err)
	}
	if _, err := NewDecryptor(DecryptorConfig{}); !errors.Is(err, ErrEncryptionKeyRequired) {
		t.Fatalf("empty decryptor: got %v", err)
	}
	if _, err := NewScryptEncryptor("x", MaxScryptWorkFactor+1); !errors.Is(err, ErrInvalidWorkFactor) {
		t.Fatalf("work factor: got %v", err)
	}
}

func TestScryptWorkFactorCappedForDecryption(t *testing.T) {
	if _, err := NewScryptEncryptor("pw", MaxScryptWorkFactor+1); !errors.Is(err, ErrInvalidWorkFactor) {
		t.Fatalf("encryptor above cap: got %v", err)
	}

	// A foreign file demanding more than MaxScryptWorkFactor must be refused before any
	// key derivation. The header is hand-crafted so the test never pays for scrypt.
	b64 := base64.RawStdEncoding.EncodeToString
	header := fmt.Sprintf("age-encryption.org/v1\n-> scrypt %s %d\n%s\n--- %s\n",
		b64(make([]byte, 16)), MaxScryptWorkFactor+1, b64(make([]byte, 32)), b64(make([]byte, 32)))
	ciphertext := append([]byte(header), make([]byte, 64)...)

	start := time.Now()
	dec, _ := NewDecryptor(DecryptorConfig{Passphrase: "pw"})
	_, err := decrypt(dec, ciphertext)
	if !errors.Is(err, ErrDecryptionFailed) || !strings.Contains(err.Error(), "work factor") {
		t.Fatalf("expected ErrDecryptionFailed for oversized work factor, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("oversized work factor was derived instead of rejected (%v)", elapsed)
	}
}

func TestDecryptMissingFinalChunk(t *testing.T) {
	id, r := genKey(t)
	enc, _ := NewX25519Encryptor([]string{r})
	const chunk = 64 * 1024
	const tail = 100
	ciphertext := encrypt(t, enc, randomPayload(t, 2*chunk+tail))
	dec, _ := NewDecryptor(DecryptorConfig{Identity: id})

	// Drop exactly the final (short, last-flagged) chunk: tail plaintext + 16-byte tag.
	truncated := ciphertext[:len(ciphertext)-(tail+16)]
	if _, err := decrypt(dec, truncated); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("missing final chunk: expected ErrDecryptionFailed, got %v", err)
	}
}

func TestIdentityFilePermissionWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not applicable")
	}
	id, _ := genKey(t)
	path := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o640, true}, {0o644, true}} {
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		if _, err := NewDecryptor(DecryptorConfig{IdentityFile: path, Logger: logger}); err != nil {
			t.Fatalf("mode %v must still load: %v", tc.mode, err)
		}
		if got := strings.Contains(logs.String(), "readable by group or others"); got != tc.warn {
			t.Fatalf("mode %v: warning=%v, want %v (logs: %q)", tc.mode, got, tc.warn, logs.String())
		}
		if strings.Contains(logs.String(), "AGE-SECRET-KEY") {
			t.Fatal("warning leaks key material")
		}
	}
}

func TestIdentityFile(t *testing.T) {
	id, r := genKey(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	content := "# created: now\n# public key: " + r + "\n\n" + id + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	enc, _ := NewX25519Encryptor([]string{r})
	ciphertext := encrypt(t, enc, []byte("payload"))
	dec, err := NewDecryptor(DecryptorConfig{IdentityFile: path, Passphrase: "also-configured"})
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	got, err := decrypt(dec, ciphertext)
	if err != nil || string(got) != "payload" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}

	bad := filepath.Join(dir, "bad.txt")
	_ = os.WriteFile(bad, []byte("# comment\nAGE-SECRET-KEY-1NOTVALID\n"), 0o600)
	_, err = NewDecryptor(DecryptorConfig{IdentityFile: bad})
	if !errors.Is(err, ErrInvalidIdentity) || strings.Contains(err.Error(), "NOTVALID") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("bad identity file: got %v", err)
	}

	if _, err := NewDecryptor(DecryptorConfig{IdentityFile: filepath.Join(dir, "missing")}); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("missing identity file: got %v", err)
	}
	if _, err := NewDecryptor(DecryptorConfig{Identity: "# only comments\n"}); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("empty identity: got %v", err)
	}
}
