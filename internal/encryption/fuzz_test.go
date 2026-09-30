package encryption

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Test-only X25519 key pairs, fixed so that fuzz inputs are reproducible across
// the fuzzing worker processes. They protect nothing.
const (
	fuzzIdentity   = "AGE-SECRET-KEY-19KU4NVLJA700YYCF459KLN2M839AC0957NS949L9YMK85NJVZVGSHPFVTR" //nolint:gosec // G101: a test-only key.
	fuzzRecipient  = "age1za53er7jnkn9kznlgxhu6w7uqzken0lrxu4jjgrq5e9ms6xlp9tq8x028d"
	otherIdentity  = "AGE-SECRET-KEY-1HCM9KT452545Z486JEVHHQMRWRZD7EJC33PRNHVZL37LKHNGDGAQND4MPZ" //nolint:gosec // G101: a test-only key.
	otherRecipient = "age1fqa7lp48axu29pv9l0tcqn3mgukjv2u3x6vknr4g9x0kl7npt40qj4n7rn"
)

// ageChunkSize is the plaintext size of an age payload chunk; each chunk carries a
// 16-byte Poly1305 tag.
const ageChunkSize = 64 << 10

// fuzzDecryptor returns a Decryptor for identity.
func fuzzDecryptor(t testing.TB, identity string) *Decryptor {
	d, err := NewDecryptor(DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fuzzEncrypt encrypts plaintext to recipients, writing it in pieces of step bytes.
func fuzzEncrypt(t testing.TB, plaintext []byte, step int, recipients ...string) []byte {
	enc, err := NewX25519Encryptor(recipients)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for rest := plaintext; len(rest) > 0; {
		n := min(step, len(rest))
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzDecrypt checks that decrypting arbitrary bytes never panics and that every
// failure wraps ErrDecryptionFailed (the source itself never fails here).
func FuzzDecrypt(f *testing.F) {
	valid := fuzzEncrypt(f, []byte("backup archive"), 7, fuzzRecipient)
	large := fuzzEncrypt(f, bytes.Repeat([]byte{0xab}, ageChunkSize+100), 4096, fuzzRecipient)
	f.Add(valid)
	f.Add(valid[:len(valid)-1])
	f.Add(large)
	f.Add(large[:len(large)-200])
	f.Add(fuzzEncrypt(f, nil, 1, otherRecipient))
	f.Add(fuzzEncrypt(f, []byte("two"), 1, otherRecipient, fuzzRecipient))
	f.Add([]byte(""))
	f.Add([]byte("age-encryption.org/v1\n"))
	f.Add([]byte("age-encryption.org/v1\n-> X25519 AAAA\nAAAA\n--- AAAA\n"))
	f.Add([]byte("age-encryption.org/v1\n-> scrypt c2FsdA 18\nAAAA\n--- AAAA\n"))
	var armored bytes.Buffer
	aw := armor.NewWriter(&armored)
	_, _ = aw.Write(valid)
	_ = aw.Close()
	f.Add(armored.Bytes())

	dec := fuzzDecryptor(f, fuzzIdentity)
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := dec.Decrypt(bytes.NewReader(data))
		if err != nil {
			if !errors.Is(err, ErrDecryptionFailed) {
				t.Fatalf("Decrypt: error %v does not wrap ErrDecryptionFailed", err)
			}
			return
		}
		if _, err := io.Copy(io.Discard, r); err != nil && !errors.Is(err, ErrDecryptionFailed) {
			t.Fatalf("read: error %v does not wrap ErrDecryptionFailed", err)
		}
	})
}

// cutReader serves r until limit bytes were read, then fails with errCut.
type cutReader struct {
	r     io.Reader
	limit int
}

var errCut = errors.New("source cut")

func (c *cutReader) Read(p []byte) (int, error) {
	if c.limit <= 0 {
		return 0, errCut
	}
	if len(p) > c.limit {
		p = p[:c.limit]
	}
	n, err := c.r.Read(p)
	c.limit -= n
	return n, err
}

// FuzzRoundTrip checks that encrypt then decrypt returns the plaintext, that the
// wrong identity fails, and that decryption streams: the first chunk is readable
// once the source has delivered it, without the rest of the ciphertext.
func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("mongodump archive"), uint8(0), uint16(3))
	f.Add([]byte{}, uint8(0), uint16(1))
	f.Add([]byte{0}, uint8(64), uint16(65535))
	f.Add([]byte("x"), uint8(130), uint16(4096))
	f.Fuzz(func(t *testing.T, data []byte, kib uint8, step uint16) {
		plaintext := make([]byte, len(data)+int(kib)*1024)
		for i := range plaintext {
			if len(data) > 0 {
				plaintext[i] = data[i%len(data)]
			}
		}
		ciphertext := fuzzEncrypt(t, plaintext, 1+int(step), fuzzRecipient)

		got, err := decrypt(fuzzDecryptor(t, fuzzIdentity), ciphertext)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatalf("round trip of %d bytes: err = %v, equal = %v", len(plaintext), err, bytes.Equal(got, plaintext))
		}
		if _, err := decrypt(fuzzDecryptor(t, otherIdentity), ciphertext); !errors.Is(err, ErrDecryptionFailed) {
			t.Fatalf("wrong identity: err = %v; want ErrDecryptionFailed", err)
		}

		if len(plaintext) <= ageChunkSize {
			return
		}
		chunks := (len(plaintext) + ageChunkSize - 1) / ageChunkSize
		header := len(ciphertext) - 16 - len(plaintext) - chunks*16 // nonce and tags
		src := &cutReader{r: bytes.NewReader(ciphertext), limit: header + 16 + ageChunkSize + 16 + 1}
		r, err := fuzzDecryptor(t, fuzzIdentity).Decrypt(src)
		if err != nil {
			t.Fatalf("Decrypt with the first chunk only: %v", err)
		}
		first := make([]byte, ageChunkSize)
		if _, err := io.ReadFull(r, first); err != nil || !bytes.Equal(first, plaintext[:ageChunkSize]) {
			t.Fatalf("first chunk not readable from a partial source: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); !errors.Is(err, errCut) {
			t.Fatalf("reading past the cut: err = %v; want the source error", err)
		}
	})
}

// TestFuzzKeysMatch guards the hard-coded fuzz key pairs.
func TestFuzzKeysMatch(t *testing.T) {
	for id, want := range map[string]string{fuzzIdentity: fuzzRecipient, otherIdentity: otherRecipient} {
		parsed, err := age.ParseX25519Identity(id)
		if err != nil || parsed.Recipient().String() != want {
			t.Fatalf("fuzz key pair mismatch: %v", err)
		}
	}
}
