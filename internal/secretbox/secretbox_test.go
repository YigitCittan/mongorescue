package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var here = At("connections", "con_1", "uri")

func TestSealOpenRoundTrip(t *testing.T) {
	b := newBox(t)
	for _, plain := range []string{"", "mongodb://u:p@h/db", strings.Repeat("x", 4096), "ünïcödé", "sb1:looks-sealed", "sb2:AAAA"} {
		sealed, err := b.Seal(here, plain)
		if err != nil {
			t.Fatal(err)
		}
		if !IsSealed(sealed) || strings.Contains(sealed, "p@h") && plain != "" {
			t.Fatalf("sealed value leaks plaintext or lacks prefix: %q", sealed)
		}
		got, err := b.Open(here, sealed)
		if err != nil || got != plain {
			t.Fatalf("Open = %q, %v; want %q", got, err, plain)
		}
	}
	a, _ := b.Seal(here, "same")
	c, _ := b.Seal(here, "same")
	if a == c {
		t.Fatal("two seals of the same plaintext must differ (random nonce)")
	}
}

func TestSealedValuesAreBoundToTheirLocation(t *testing.T) {
	b := newBox(t)
	sealed, err := b.Seal(here, "mongodb://prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []Binding{
		At("connections", "con_2", "uri"),                        // another record
		At("connections", "con_1", "description"),                // another field
		At("storage_targets", "con_1", "uri"),                    // another table
		At("connections|con_1", "uri", "x"),                      // shifted separators
		At("connections", "con_1|uri", "x"),                      // shifted separators
		{Table: "connections", RecordID: "con_1", Field: "uri "}, // near miss
	} {
		if _, err := b.Open(other, sealed); !errors.Is(err, ErrDecrypt) {
			t.Errorf("Open at %s = %v; want ErrDecrypt", other, err)
		}
	}
	for _, bad := range []Binding{{}, At("t", "", "f"), At("", "id", "f"), At("t", "id", "")} {
		if _, err := b.Seal(bad, "x"); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("Seal at %+v = %v; want ErrInvalidBinding", bad, err)
		}
		if _, err := b.Open(bad, sealed); !errors.Is(err, ErrInvalidBinding) {
			t.Errorf("Open at %+v = %v; want ErrInvalidBinding", bad, err)
		}
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	b := newBox(t)
	sealed, _ := b.Seal(here, "bot-token-123")
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, Prefix))
	for i := range raw {
		mut := bytes.Clone(raw)
		mut[i] ^= 0x01
		_, err := b.Open(here, Prefix+base64.StdEncoding.EncodeToString(mut))
		if i == 0 {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("flipped version byte: err = %v; want ErrMalformed", err)
			}
			continue
		}
		if !errors.Is(err, ErrDecrypt) {
			t.Fatalf("flipped byte %d: err = %v; want ErrDecrypt", i, err)
		}
	}
	if _, err := b.Open(here, Prefix+base64.StdEncoding.EncodeToString(raw[:len(raw)-1])); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated: err = %v; want ErrDecrypt", err)
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	sealed, _ := newBox(t).Seal(here, "secret")
	if _, err := newBox(t).Open(here, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v; want ErrDecrypt", err)
	}
}

func TestOpenMalformed(t *testing.T) {
	b := newBox(t)
	legacy := sealLegacy(t, b, "old")
	for _, s := range []string{"", "plaintext", Prefix, Prefix + "!!!", Prefix + "AAAA", legacy} {
		if _, err := b.Open(here, s); !errors.Is(err, ErrMalformed) {
			t.Errorf("Open(%q) = %v; want ErrMalformed", s, err)
		}
	}
	current, _ := b.Seal(here, "new")
	for _, s := range []string{"plaintext", current, LegacyPrefix + "AAAA"} {
		if _, err := b.OpenLegacy(s); !errors.Is(err, ErrMalformed) {
			t.Errorf("OpenLegacy(%q) = %v; want ErrMalformed", s, err)
		}
	}
}

// sealLegacy produces an "sb1:" value the way earlier releases did.
func sealLegacy(t *testing.T, b *Box, plain string) string {
	t.Helper()
	nonce := make([]byte, b.aead.NonceSize())
	buf := append([]byte{legacyFormatVersion}, nonce...)
	return LegacyPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(buf, nonce, []byte(plain), []byte{legacyFormatVersion}))
}

func TestOpenLegacy(t *testing.T) {
	b := newBox(t)
	got, err := b.OpenLegacy(sealLegacy(t, b, "mongodb://legacy"))
	if err != nil || got != "mongodb://legacy" || !IsLegacySealed(LegacyPrefix) || IsSealed(LegacyPrefix) {
		t.Fatalf("OpenLegacy = %q, %v", got, err)
	}
	if _, err := newBox(t).OpenLegacy(sealLegacy(t, b, "x")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("legacy value with another key = %v; want ErrDecrypt", err)
	}
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir(existing) = %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("syncDir(missing) must fail")
		}
	}
}

func TestKeyParsing(t *testing.T) {
	if _, err := New(make([]byte, 16)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("New(16 bytes) = %v", err)
	}
	key, _ := GenerateKey()
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := ParseKey("  " + enc.EncodeToString(key) + "\n")
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("ParseKey(%v) = %v", enc, err)
		}
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		if _, err := ParseKey(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseKey(%q) = %v; want ErrInvalidKey", bad, err)
		}
	}
}

func TestLoadKeyFromEnv(t *testing.T) {
	key, _ := GenerateKey()
	file := filepath.Join(t.TempDir(), KeyFileName)
	got, err := LoadKey(KeySource{Env: EncodeKey(key), File: file}, nil)
	if err != nil || !got.FromEnv || got.Created || !bytes.Equal(got.Key, key) {
		t.Fatalf("LoadKey(env) = %+v, %v", got, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an env key must not create a key file")
	}
	if _, err := LoadKey(KeySource{Env: "short"}, nil); !errors.Is(err, ErrInvalidKey) || strings.Contains(err.Error(), "short") {
		t.Fatalf("invalid env key error = %v (must not echo the value)", err)
	}
}

func TestLoadKeyCreatesAndReusesFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nested", KeyFileName)
	logger := slog.New(slog.DiscardHandler)
	first, err := LoadKey(KeySource{File: file}, logger)
	if err != nil || !first.Created {
		t.Fatalf("first LoadKey = %+v, %v", first, err)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(file)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, %v; want 0600", info, statErr)
		}
	}
	second, err := LoadKey(KeySource{File: file}, logger)
	if err != nil || second.Created || !bytes.Equal(first.Key, second.Key) {
		t.Fatalf("second LoadKey = %+v, %v; want the same key", second, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".secret.key.tmp-*")); len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}

	if err := os.WriteFile(file, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(KeySource{File: file}, logger); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("corrupt key file = %v; want ErrInvalidKey", err)
	}
}

func TestLoadKeyWarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	file := filepath.Join(t.TempDir(), KeyFileName)
	key, _ := GenerateKey()
	if err := os.WriteFile(file, []byte(EncodeKey(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if _, err := LoadKey(KeySource{File: file}, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), EncodeKey(key)) {
		t.Fatalf("want a warning without key material; logs: %s", logs.String())
	}
}
