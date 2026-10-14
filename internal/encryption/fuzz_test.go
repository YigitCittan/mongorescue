package encryption

import (
	"bytes"
	"errors"
	"io"
	"sync"
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

// fataler is the part of testing.TB the fuzz seed helpers use. Fuzz targets pass
// their *testing.F, which in libFuzzer builds (ClusterFuzzLite, go-118-fuzz-build)
// is a stand-in that does not implement all of testing.TB.
type fataler interface{ Fatal(args ...any) }

// fuzzFail stops a fuzz target or seed helper whose setup failed. Under go test
// Fatal ends the goroutine and the panic is never reached. In libFuzzer builds
// (ClusterFuzzLite, go-118-fuzz-build) the *testing.F stand-in's Fatal returns, so
// the panic reports the real error instead of a nil dereference further on.
func fuzzFail(t fataler, err error) {
	t.Fatal(err) //nolint:revive // Fatal returns in libFuzzer builds; the panic is for them.
	panic(err)
}

// fuzzDecryptor returns a Decryptor for identity.
func fuzzDecryptor(t fataler, identity string) *Decryptor {
	d, err := NewDecryptor(DecryptorConfig{Identity: identity})
	if err != nil {
		fuzzFail(t, err)
	}
	return d
}

// encryptSeed encrypts plaintext to recipients, writing it in pieces of step bytes.
func encryptSeed(plaintext []byte, step int, recipients ...string) ([]byte, error) {
	enc, err := NewX25519Encryptor(recipients)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	if err != nil {
		return nil, err
	}
	for rest := plaintext; len(rest) > 0; {
		n := min(step, len(rest))
		if _, err = w.Write(rest[:n]); err != nil {
			return nil, err
		}
		rest = rest[n:]
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fuzzEncrypt is encryptSeed that fails t.
func fuzzEncrypt(t fataler, plaintext []byte, step int, recipients ...string) []byte {
	b, err := encryptSeed(plaintext, step, recipients...)
	if err != nil {
		fuzzFail(t, err)
	}
	return b
}

// decryptFuzzSeeds are the seed ciphertexts and the decryptor of FuzzDecrypt.
type decryptFuzzSeeds struct {
	valid, large, empty, two []byte
	dec                      *Decryptor
}

// decryptSeeds builds them once per process: libFuzzer builds run the whole Fuzz
// function for every input, and the large seed alone encrypts 64 KiB.
var decryptSeeds = sync.OnceValues(func() (*decryptFuzzSeeds, error) {
	s := &decryptFuzzSeeds{}
	var err error
	if s.valid, err = encryptSeed([]byte("backup archive"), 7, fuzzRecipient); err != nil {
		return nil, err
	}
	if s.large, err = encryptSeed(bytes.Repeat([]byte{0xab}, ageChunkSize+100), 4096, fuzzRecipient); err != nil {
		return nil, err
	}
	if s.empty, err = encryptSeed(nil, 1, otherRecipient); err != nil {
		return nil, err
	}
	if s.two, err = encryptSeed([]byte("two"), 1, otherRecipient, fuzzRecipient); err != nil {
		return nil, err
	}
	if s.dec, err = NewDecryptor(DecryptorConfig{Identity: fuzzIdentity}); err != nil {
		return nil, err
	}
	return s, nil
})

// FuzzDecrypt checks that decrypting arbitrary bytes never panics and that every
// failure wraps ErrDecryptionFailed (the source itself never fails here).
func FuzzDecrypt(f *testing.F) {
	seeds, err := decryptSeeds()
	if err != nil {
		fuzzFail(f, err)
	}
	valid, large := seeds.valid, seeds.large
	f.Add(valid)
	f.Add(valid[:len(valid)-1])
	f.Add(large)
	f.Add(large[:len(large)-200])
	f.Add(seeds.empty)
	f.Add(seeds.two)
	f.Add([]byte(""))
	f.Add([]byte("age-encryption.org/v1\n"))
	f.Add([]byte("age-encryption.org/v1\n-> X25519 AAAA\nAAAA\n--- AAAA\n"))
	f.Add([]byte("age-encryption.org/v1\n-> scrypt c2FsdA 18\nAAAA\n--- AAAA\n"))
	var armored bytes.Buffer
	aw := armor.NewWriter(&armored)
	_, _ = aw.Write(valid)
	_ = aw.Close()
	f.Add(armored.Bytes())

	dec := seeds.dec
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
		if _, wrongErr := decrypt(fuzzDecryptor(t, otherIdentity), ciphertext); !errors.Is(wrongErr, ErrDecryptionFailed) {
			t.Fatalf("wrong identity: err = %v; want ErrDecryptionFailed", wrongErr)
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
