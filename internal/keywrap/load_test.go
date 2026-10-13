package keywrap_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

var testSpec = keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "alias/mongorescue"}

// fastRetry retries three times without waiting.
var fastRetry = keywrap.Retry{Attempts: 3, Initial: time.Millisecond, Max: time.Millisecond}

func load(dir string, w keywrap.Wrapper) (*keywrap.Loaded, error) {
	return keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: testSpec, Wrapper: w, Retry: fastRetry,
		Sleep: keywraptest.NoSleep, Logger: slog.New(slog.DiscardHandler)})
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	ok, err := secretbox.KeyFileExists(path)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func writeKey(t *testing.T, path string) []byte {
	t.Helper()
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err = secretbox.WriteKeyFile(path, key); err != nil {
		t.Fatal(err)
	}
	return key
}

// assertNoPlaintext fails when a file of dir contains key in any form.
func assertNoPlaintext(t *testing.T, dir string, key []byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if bytes.Contains(data, key) || bytes.Contains(data, []byte(secretbox.EncodeKey(key))) {
			t.Errorf("%s holds the plaintext key", e.Name())
		}
	}
}

func TestLoadFirstStartStoresOnlyTheWrappedKey(t *testing.T) {
	dir := t.TempDir()
	kms := keywraptest.New("kms")
	first, err := load(dir, kms)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || len(first.Migrated) != 0 || len(first.Key) != secretbox.KeySize {
		t.Fatalf("first start = %+v", first)
	}
	if exists(t, filepath.Join(dir, secretbox.KeyFileName)) {
		t.Fatal("a plaintext secret.key was written")
	}
	wrapped := filepath.Join(dir, keywrap.WrappedKeyFileName)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(wrapped)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("secret.key.wrapped mode = %v, %v", info, err)
		}
	}
	assertNoPlaintext(t, dir, first.Key)
	f, err := keywrap.ReadFile(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := secretbox.Fingerprint(first.Key)
	if f.Provider != testSpec.Provider || f.KeyURI != testSpec.KeyURI || f.KeyID != kms.KeyID() || f.Fingerprint != fp || f.Version != keywrap.FileVersion {
		t.Errorf("wrapped file = %+v", f)
	}

	again, err := load(dir, kms)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || !bytes.Equal(again.Key, first.Key) || again.Attempts != 1 {
		t.Fatalf("restart = %+v", again)
	}
	if changes, _ := keywrap.ReadJournal(dir); len(changes) != 0 {
		t.Errorf("a new key is not a wrapping change: %+v", changes)
	}
}

func TestLoadMigratesPlaintextKeyFiles(t *testing.T) {
	dir := t.TempDir()
	cur := writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
	next := writeKey(t, filepath.Join(dir, secretbox.NextKeyFileName))
	prev := writeKey(t, filepath.Join(dir, secretbox.PreviousKeyFileName))
	kms := keywraptest.New("kms")

	got, err := load(dir, kms)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Key, cur) || got.Created {
		t.Fatalf("Load = %+v", got)
	}
	if want := []string{secretbox.KeyFileName, secretbox.NextKeyFileName, secretbox.PreviousKeyFileName}; strings.Join(got.Migrated, ",") != strings.Join(want, ",") {
		t.Errorf("Migrated = %v; want %v", got.Migrated, want)
	}
	for _, name := range []string{secretbox.KeyFileName, secretbox.NextKeyFileName, secretbox.PreviousKeyFileName} {
		if exists(t, filepath.Join(dir, name)) {
			t.Errorf("plaintext %s still exists", name)
		}
		if !exists(t, filepath.Join(dir, name+keywrap.Suffix)) {
			t.Errorf("%s was not wrapped", name)
		}
	}
	for _, k := range [][]byte{cur, next, prev} {
		assertNoPlaintext(t, dir, k)
	}
	codec := keywrap.NewCodec(testSpec, kms).WithRetry(fastRetry, keywraptest.NoSleep)
	for name, want := range map[string][]byte{secretbox.NextKeyFileName: next, secretbox.PreviousKeyFileName: prev} {
		k, err := codec.ReadKey(context.Background(), filepath.Join(dir, name+keywrap.Suffix))
		if err != nil || !bytes.Equal(k, want) {
			t.Errorf("ReadKey(%s) = %v", name, err)
		}
	}
	changes, err := keywrap.ReadJournal(dir)
	if err != nil || len(changes) != 1 {
		t.Fatalf("journal = %+v, %v", changes, err)
	}
	c := changes[0]
	fp, _ := secretbox.Fingerprint(cur)
	if c.Action != keywrap.ActionWrapped || c.Actor != keywrap.ActorSystem || c.Fingerprint != fp || len(c.Files) != 3 || c.KeyURI != testSpec.KeyURI {
		t.Errorf("journal entry = %+v", c)
	}
	if !strings.Contains(c.Detail(), "aws-kms:alias/mongorescue") {
		t.Errorf("Detail = %q", c.Detail())
	}
}

func TestLoadFinishesAnInterruptedMigration(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, secretbox.KeyFileName)
	key := writeKey(t, plain)
	kms := keywraptest.New("kms")
	if _, err := load(dir, kms); err != nil {
		t.Fatal(err)
	}
	// A crash after the wrapped file was written, before the plaintext one went.
	if err := secretbox.WriteKeyFile(plain, key); err != nil {
		t.Fatal(err)
	}
	got, err := load(dir, kms)
	if err != nil || !bytes.Equal(got.Key, key) {
		t.Fatalf("Load = %v", err)
	}
	if exists(t, plain) {
		t.Fatal("the leftover plaintext secret.key was kept")
	}

	// A different plaintext key is never silently dropped.
	other := writeKey(t, plain)
	if _, err = load(dir, kms); !errors.Is(err, keywrap.ErrConflict) {
		t.Fatalf("Load with a conflicting plaintext key = %v; want ErrConflict", err)
	}
	if k, err := secretbox.ReadKeyFile(plain); err != nil || !bytes.Equal(k, other) {
		t.Fatal("the conflicting plaintext key was touched")
	}
}

func TestLoadRetriesAnUnreachableKMS(t *testing.T) {
	dir := t.TempDir()
	kms := keywraptest.New("kms")
	first, err := load(dir, kms)
	if err != nil {
		t.Fatal(err)
	}
	kms.SetDown(2)
	got, err := load(dir, kms)
	if err != nil || got.Attempts != 3 || !bytes.Equal(got.Key, first.Key) {
		t.Fatalf("Load after two failures = %+v, %v", got, err)
	}

	kms.SetDown(-1)
	_, err = load(dir, kms)
	if !errors.Is(err, keywrap.ErrUnavailable) || errors.Is(err, keywrap.ErrUnwrap) {
		t.Fatalf("Load with the KMS down = %v; want ErrUnavailable", err)
	}
	for _, want := range []string{"after 3 attempt(s)", "kms:Decrypt", "never falls back", "aws-kms:alias/mongorescue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if exists(t, filepath.Join(dir, secretbox.KeyFileName)) {
		t.Fatal("a plaintext key appeared")
	}
}

func TestLoadNeverGivesUpThePlaintextKeyWhenWrappingFails(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, secretbox.KeyFileName)
	key := writeKey(t, plain)
	kms := keywraptest.New("kms")
	kms.SetDown(-1)
	if _, err := load(dir, kms); !errors.Is(err, keywrap.ErrUnavailable) {
		t.Fatalf("Load = %v; want ErrUnavailable", err)
	}
	if k, err := secretbox.ReadKeyFile(plain); err != nil || !bytes.Equal(k, key) {
		t.Fatal("the plaintext key was removed although it could not be wrapped")
	}
	if exists(t, plain+keywrap.Suffix) {
		t.Fatal("a wrapped key was written")
	}
}

func TestLoadRefusedUnwrapFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := load(dir, keywraptest.New("kms")); err != nil {
		t.Fatal(err)
	}
	wrong := keywraptest.New("another key")
	_, err := load(dir, wrong)
	if !errors.Is(err, keywrap.ErrUnwrap) {
		t.Fatalf("Load with another KMS key = %v; want ErrUnwrap", err)
	}
	if n := wrong.Unwraps.Load(); n != 1 {
		t.Errorf("a refused unwrap was retried: %d calls", n)
	}
}

func TestLoadDetectsTamperedFiles(t *testing.T) {
	kms := keywraptest.New("kms")
	tamper := func(t *testing.T, edit func(f map[string]any)) error {
		t.Helper()
		dir := t.TempDir()
		if _, err := load(dir, kms); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, keywrap.WrappedKeyFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]any
		if err = json.Unmarshal(data, &f); err != nil {
			t.Fatal(err)
		}
		edit(f)
		if data, err = json.Marshal(f); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = load(dir, kms)
		return err
	}
	t.Run("ciphertext", func(t *testing.T) {
		err := tamper(t, func(f map[string]any) {
			ct, err := base64.StdEncoding.DecodeString(f["ciphertext"].(string))
			if err != nil {
				t.Fatal(err)
			}
			ct[len(ct)-1] ^= 1
			f["ciphertext"] = base64.StdEncoding.EncodeToString(ct)
		})
		if !errors.Is(err, keywrap.ErrUnwrap) {
			t.Fatalf("tampered ciphertext = %v", err)
		}
	})
	t.Run("fingerprint", func(t *testing.T) {
		err := tamper(t, func(f map[string]any) { f["fingerprint"] = strings.Repeat("0", 32) })
		if !errors.Is(err, keywrap.ErrWrongKey) {
			t.Fatalf("tampered fingerprint = %v; want ErrWrongKey", err)
		}
	})
	t.Run("key uri", func(t *testing.T) {
		err := tamper(t, func(f map[string]any) { f["key_uri"] = "alias/other" })
		if !errors.Is(err, keywrap.ErrKeyMismatch) || !strings.Contains(err.Error(), "rewrap") {
			t.Fatalf("other key URI = %v; want ErrKeyMismatch", err)
		}
	})
	t.Run("format", func(t *testing.T) {
		err := tamper(t, func(f map[string]any) { f["version"] = 9 })
		if !errors.Is(err, keywrap.ErrMalformed) {
			t.Fatalf("unknown version = %v; want ErrMalformed", err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, keywrap.WrappedKeyFileName), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := load(dir, kms); !errors.Is(err, keywrap.ErrMalformed) {
			t.Fatalf("garbage = %v; want ErrMalformed", err)
		}
	})
}

func TestLoadNeedsItsParameters(t *testing.T) {
	if _, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: t.TempDir()}); err == nil {
		t.Fatal("Load without a spec and wrapper succeeded")
	}
}

func TestCodecWrapsWhatItWrites(t *testing.T) {
	dir := t.TempDir()
	kms := keywraptest.New("kms")
	codec := keywrap.NewCodec(testSpec, kms).WithRetry(fastRetry, keywraptest.NoSleep)
	if codec.Spec() != testSpec {
		t.Fatal("Spec")
	}
	key, _ := secretbox.GenerateKey()
	path := filepath.Join(dir, secretbox.NextKeyFileName+keywrap.Suffix)
	if err := codec.WriteKey(context.Background(), path, key); err != nil {
		t.Fatal(err)
	}
	assertNoPlaintext(t, dir, key)
	got, err := codec.ReadKey(context.Background(), path)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ReadKey = %v", err)
	}
	if _, err = codec.ReadKey(context.Background(), filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadKey of a missing file = %v", err)
	}
	other := keywrap.NewCodec(keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: "alias/other"}, kms)
	if _, err = other.ReadKey(context.Background(), path); !errors.Is(err, keywrap.ErrKeyMismatch) {
		t.Fatalf("ReadKey with another spec = %v", err)
	}
	kms.SetDown(-1)
	if err = codec.WriteKey(context.Background(), path, key); !errors.Is(err, keywrap.ErrUnavailable) {
		t.Fatalf("WriteKey with the KMS down = %v", err)
	}
	if err = codec.WriteKey(context.Background(), path, []byte("short")); err == nil {
		t.Fatal("WriteKey of an invalid key succeeded")
	}
}

func TestInstrumentedReplaysEarlyCalls(t *testing.T) {
	kms := keywraptest.New("kms")
	inst := keywrap.Instrument(kms)
	ct, err := inst.Wrap(context.Background(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	kms.SetDown(1)
	_, _ = inst.Unwrap(context.Background(), ct)
	type call struct {
		op, provider string
		failed       bool
	}
	var calls []call
	inst.SetObserver(func(op, provider string, _ time.Duration, err error) {
		calls = append(calls, call{op, provider, err != nil})
	})
	if _, err = inst.Unwrap(context.Background(), ct); err != nil {
		t.Fatal(err)
	}
	want := []call{{keywrap.OpWrap, keywrap.ProviderAWSKMS, false}, {keywrap.OpUnwrap, keywrap.ProviderAWSKMS, true}, {keywrap.OpUnwrap, keywrap.ProviderAWSKMS, false}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v; want %+v", i, calls[i], want[i])
		}
	}
	if inst.KeyID() != kms.KeyID() || inst.Provider() != kms.Provider() {
		t.Error("Instrumented does not pass KeyID and Provider through")
	}
}
