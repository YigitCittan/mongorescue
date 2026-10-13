package recoverykit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

var kitSpec = keywrap.Spec{Provider: keywrap.ProviderGCPKMS, KeyURI: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}

// wrappedFixture is a fixture whose data directory keeps secret.key (and a
// previous key) wrapped by a fake KMS.
type wrappedFixture struct {
	*fixture
	dir   string
	prev  []byte
	codec *keywrap.Codec
}

func newWrappedFixture(t *testing.T) *wrappedFixture {
	t.Helper()
	dir := t.TempDir()
	kms := keywraptest.New("kms")
	cur, _ := secretbox.GenerateKey()
	prev, _ := secretbox.GenerateKey()
	if err := secretbox.WriteKeyFile(filepath.Join(dir, secretbox.KeyFileName), cur); err != nil {
		t.Fatal(err)
	}
	if err := secretbox.WriteKeyFile(filepath.Join(dir, secretbox.PreviousKeyFileName), prev); err != nil {
		t.Fatal(err)
	}
	loaded, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: kitSpec, Wrapper: kms,
		Retry: keywrap.Retry{Attempts: 1}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.key = loaded.Key
	codec := keywrap.NewCodec(kitSpec, kms)
	f.svc, err = recoverykit.New(recoverykit.Config{
		SecretKey: loaded.Key, Settings: f.settings, Targets: f.targets, WorkFactor: 10,
		PreviousKeyFile: filepath.Join(dir, secretbox.PreviousKeyFileName+keywrap.Suffix),
		KeyWrap: &recoverykit.KeyWrap{Spec: kitSpec, KeyID: kms.KeyID(), CurrentFile: filepath.Join(dir, keywrap.WrappedKeyFileName),
			ReadKey: codec.ReadKey},
		Now: func() time.Time { return time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &wrappedFixture{fixture: f, dir: dir, prev: prev, codec: codec}
}

func (w *wrappedFixture) kit(t *testing.T, opts recoverykit.PrepareOptions) map[string]string {
	t.Helper()
	kit, err := w.svc.PrepareWith(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err = w.svc.Seal(&sealed, kit, passphrase); err != nil {
		t.Fatal(err)
	}
	return open(t, sealed.Bytes(), passphrase)
}

func TestWrappedKitCarriesTheWrappedKeyOnly(t *testing.T) {
	w := newWrappedFixture(t)
	if !w.svc.KeyWrapped() {
		t.Fatal("KeyWrapped = false")
	}
	files := w.kit(t, recoverykit.PrepareOptions{IncludePreviousKey: true})
	if _, ok := files[recoverykit.SecretKeyName]; ok {
		t.Fatal("the kit carries the plaintext secret.key")
	}
	if _, ok := files[recoverykit.PreviousKeyName]; ok {
		t.Fatal("the kit carries the plaintext secret.key.previous")
	}
	for name, want := range map[string][]byte{recoverykit.WrappedSecretKeyName: w.key, recoverykit.WrappedPreviousKeyName: w.prev} {
		f, err := keywrap.ParseFile([]byte(files[name]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := f.Open(context.Background(), keywraptest.New("kms"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s does not unwrap to its key: %v", name, err)
		}
	}
	for name, body := range files {
		for _, k := range [][]byte{w.key, w.prev} {
			if strings.Contains(body, secretbox.EncodeKey(k)) {
				t.Errorf("%s holds a key in plain form", name)
			}
		}
	}
	var m recoverykit.Manifest
	if err := json.Unmarshal([]byte(files[recoverykit.ManifestName]), &m); err != nil {
		t.Fatal(err)
	}
	sk := m.SecretKey
	if !sk.Wrapped || sk.File != recoverykit.WrappedSecretKeyName || sk.PreviousFile != recoverykit.WrappedPreviousKeyName ||
		sk.KeyWrap == nil || sk.KeyWrap.Env != "gcp-kms:"+kitSpec.KeyURI || sk.KeyWrap.KeyID == "" {
		t.Fatalf("manifest secret_key = %+v", sk)
	}
	readme := files[recoverykit.ReadmeName]
	for _, want := range []string{"MONGORESCUE_KEY_WRAP=gcp-kms:" + kitSpec.KeyURI, "secret.key.wrapped", "KMS key is deleted"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not mention %q", want)
		}
	}
}

func TestWrappedKitPlaintextOnRequest(t *testing.T) {
	w := newWrappedFixture(t)
	files := w.kit(t, recoverykit.PrepareOptions{IncludePreviousKey: true, PlaintextSecretKey: true})
	if strings.TrimSpace(files[recoverykit.SecretKeyName]) != secretbox.EncodeKey(w.key) ||
		strings.TrimSpace(files[recoverykit.PreviousKeyName]) != secretbox.EncodeKey(w.prev) {
		t.Fatal("the plaintext kit does not carry the plain keys")
	}
	if _, ok := files[recoverykit.WrappedSecretKeyName]; ok {
		t.Fatal("the plaintext kit carries the wrapped key too")
	}
	if !strings.Contains(files[recoverykit.ReadmeName], "asked to carry secret.key in plain form") ||
		!strings.Contains(files[recoverykit.ManifestName], `"key_wrap"`) {
		t.Fatal("README or manifest does not say the installation wraps its key")
	}
}

func TestWrappedKitRefusesAStaleWrappedFile(t *testing.T) {
	w := newWrappedFixture(t)
	next, _ := secretbox.GenerateKey()
	if err := w.svc.SetSecretKey(next, testPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Prepare(context.Background()); !errors.Is(err, recoverykit.ErrWrappedKeyStale) {
		t.Fatalf("Prepare with secret.key.wrapped of another key = %v; want ErrWrappedKeyStale", err)
	}
	// The rotation installs the new wrapped key; the kit follows.
	if err := w.codec.WriteKey(context.Background(), filepath.Join(w.dir, keywrap.WrappedKeyFileName), next); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKeyWrappingChangesTheFingerprint(t *testing.T) {
	ctx := context.Background()
	w := newWrappedFixture(t)
	wrapped, err := w.svc.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := recoverykit.New(recoverykit.Config{SecretKey: w.key, Settings: w.settings, Targets: w.targets})
	if err != nil {
		t.Fatal(err)
	}
	if fp, _ := plain.Fingerprint(ctx); fp == wrapped {
		t.Fatal("turning key wrapping on does not outdate the kit")
	}
}
