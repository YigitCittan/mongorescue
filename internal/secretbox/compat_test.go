package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadKeyEnvOverridesFile checks that MONGORESCUE_SECRET_KEY wins over an
// existing key file, leaves the file alone, and that an invalid env value is an error
// instead of a silent fallback to the file.
func TestLoadKeyEnvOverridesFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), KeyFileName)
	fileKey, _ := GenerateKey()
	content := []byte(EncodeKey(fileKey) + "\n")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	envKey, _ := GenerateKey()

	got, err := LoadKey(KeySource{Env: EncodeKey(envKey), File: file}, slog.New(slog.DiscardHandler))
	if err != nil || !got.FromEnv || got.Created || !bytes.Equal(got.Key, envKey) {
		t.Fatalf("LoadKey(env, file) = %+v, %v; want the env key", got, err)
	}
	if after, _ := os.ReadFile(file); !bytes.Equal(after, content) {
		t.Fatal("an env key must leave the key file untouched")
	}

	for _, bad := range []string{"not base64 !", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := LoadKey(KeySource{Env: bad, File: file}, nil); !errors.Is(err, ErrInvalidKey) ||
			!strings.Contains(err.Error(), "MONGORESCUE_SECRET_KEY") {
			t.Fatalf("invalid env key %q = %v; want ErrInvalidKey naming the variable", bad, err)
		}
	}

	// Every base64 flavour of the same key loads the same key.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := LoadKey(KeySource{Env: "  " + enc.EncodeToString(envKey) + "\n"}, nil)
		if err != nil || !bytes.Equal(got.Key, envKey) {
			t.Fatalf("env key in another base64 alphabet: %v", err)
		}
	}
}

// TestLoadKeyNeverReplacesADamagedKeyFile checks that a key file that exists but
// cannot be used is reported, never regenerated: a new key would make every stored
// credential undecryptable.
func TestLoadKeyNeverReplacesADamagedKeyFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty":      "",
		"whitespace": " \n",
		"garbage":    "definitely not a key",
		"short key":  base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"long key":   base64.StdEncoding.EncodeToString(make([]byte, 33)),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".key")
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadKey(KeySource{File: file}, slog.New(slog.DiscardHandler))
			if !errors.Is(err, ErrInvalidKey) || got != nil {
				t.Fatalf("LoadKey = %+v, %v; want ErrInvalidKey", got, err)
			}
			if !strings.Contains(err.Error(), file) {
				t.Fatalf("error must name the key file: %v", err)
			}
			if after, _ := os.ReadFile(file); string(after) != content {
				t.Fatal("a damaged key file must never be overwritten")
			}
		})
	}
}

// TestSealNoncesAreUnique seals the same value many times: every nonce and every
// ciphertext must differ, and each still opens.
func TestSealNoncesAreUnique(t *testing.T) {
	b := newBox(t)
	const n = 20000
	nonces := make(map[string]struct{}, n)
	sealed := make(map[string]struct{}, n)
	for i := range n {
		s, err := b.Seal(here, "mongodb://u:p@h/")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
		if err != nil || raw[0] != formatVersion {
			t.Fatalf("seal %d: malformed %q", i, s)
		}
		nonce := string(raw[1 : 1+b.aead.NonceSize()])
		if _, dup := nonces[nonce]; dup {
			t.Fatalf("nonce reused after %d seals", i)
		}
		nonces[nonce] = struct{}{}
		if _, dup := sealed[s]; dup {
			t.Fatalf("identical ciphertext after %d seals", i)
		}
		sealed[s] = struct{}{}
		if i%1000 == 0 {
			if plain, err := b.Open(here, s); err != nil || plain != "mongodb://u:p@h/" {
				t.Fatalf("open seal %d: %q, %v", i, plain, err)
			}
		}
	}
}

// TestOpenDetectsEveryTamperedByte flips each byte of a sealed payload in turn.
func TestOpenDetectsEveryTamperedByte(t *testing.T) {
	b := newBox(t)
	s, err := b.Seal(here, "a stored credential")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	for i := range raw {
		tampered := bytes.Clone(raw)
		tampered[i] ^= 0x80
		v := Prefix + base64.StdEncoding.EncodeToString(tampered)
		if plain, err := b.Open(here, v); !errors.Is(err, ErrDecrypt) && !errors.Is(err, ErrMalformed) {
			t.Fatalf("byte %d flipped: Open = %q, %v; want an error", i, plain, err)
		}
	}
	for _, cut := range []int{0, 1, 13, len(raw) - 1} {
		v := Prefix + base64.StdEncoding.EncodeToString(raw[:cut])
		if _, err := b.Open(here, v); !errors.Is(err, ErrDecrypt) && !errors.Is(err, ErrMalformed) {
			t.Fatalf("cut at %d: Open = %v; want an error", cut, err)
		}
	}
}
