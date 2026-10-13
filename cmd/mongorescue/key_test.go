package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

const (
	keyTestOld = "arn:aws:kms:eu-west-1:123456789012:alias/old"
	keyTestNew = "https://vault:8200/transit/keys/new"
)

// keyTestDir returns a data directory with secret.key wrapped by the "old" fake
// KMS, the plaintext key, and installs the fakes as the command's KMS.
func keyTestDir(t *testing.T) (string, []byte, map[string]*keywraptest.Fake) {
	t.Helper()
	fakes := map[string]*keywraptest.Fake{keyTestOld: keywraptest.New("old"), keyTestNew: keywraptest.New("new")}
	prev, prevRetry := openKeyWrapper, keyRetry
	openKeyWrapper, keyRetry = keywraptest.Opener(fakes), keywrap.Retry{Attempts: 2, Initial: time.Millisecond}
	t.Cleanup(func() { openKeyWrapper, keyRetry = prev, prevRetry })
	dir := t.TempDir()
	key, _ := secretbox.GenerateKey()
	if err := secretbox.WriteKeyFile(filepath.Join(dir, secretbox.KeyFileName), key); err != nil {
		t.Fatal(err)
	}
	if _, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: keywrap.Spec{Provider: keywrap.ProviderAWSKMS, KeyURI: keyTestOld},
		Wrapper: fakes[keyTestOld], Retry: keywrap.Retry{Attempts: 1}, Logger: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	if err := keywrap.ClearJournal(dir); err != nil {
		t.Fatal(err)
	}
	return dir, key, fakes
}

func runKeyCmd(env map[string]string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := dispatch(append([]string{"key"}, args...), func(k string) string { return env[k] }, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestKeyCommandUsage(t *testing.T) {
	if code, out, _ := runKeyCmd(nil); code != 2 || !strings.Contains(out, "rewrap") {
		t.Fatalf("no subcommand = %d %q", code, out)
	}
	if code, _, _ := runKeyCmd(nil, "help"); code != 0 {
		t.Fatalf("help = %d", code)
	}
	if code, _, errOut := runKeyCmd(nil, "explode"); code != 2 || !strings.Contains(errOut, "unknown key command") {
		t.Fatalf("unknown = %d %q", code, errOut)
	}
	if code, _, _ := runKeyCmd(nil, "status", "extra"); code != 2 {
		t.Fatalf("extra argument = %d", code)
	}
}

func TestKeyStatus(t *testing.T) {
	dir, key, _ := keyTestDir(t)
	code, out, _ := runKeyCmd(nil, "status", "--data-dir", dir)
	fp, _ := secretbox.Fingerprint(key)
	if code != 0 || !strings.Contains(out, keyTestOld) || !strings.Contains(out, fp) {
		t.Fatalf("status = %d %q", code, out)
	}
	if strings.Contains(out, secretbox.EncodeKey(key)) {
		t.Fatal("status prints the key")
	}
	code, out, _ = runKeyCmd(map[string]string{"MONGORESCUE_DATA_DIR": t.TempDir()}, "status")
	if code != 0 || !strings.Contains(out, "no secret key") {
		t.Fatalf("status of an empty directory = %d %q", code, out)
	}
}

func TestKeyUnwrapNeedsConfirmationAndAStoppedServer(t *testing.T) {
	dir, key, _ := keyTestDir(t)
	if code, _, errOut := runKeyCmd(nil, "unwrap", "--data-dir", dir); code != 2 || !strings.Contains(errOut, "--confirm-plaintext") {
		t.Fatalf("unwrap without confirmation = %d %q", code, errOut)
	}
	lock, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runKeyCmd(nil, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 1 || !strings.Contains(errOut, "stop it first") {
		t.Fatalf("unwrap while running = %d %q", code, errOut)
	}
	_ = lock.Release()

	code, out, errOut := runKeyCmd(nil, "unwrap", "--data-dir", dir, "--confirm-plaintext")
	if code != 0 || !strings.Contains(out, "Unset MONGORESCUE_KEY_WRAP") {
		t.Fatalf("unwrap = %d %q %q", code, out, errOut)
	}
	got, err := secretbox.ReadKeyFile(filepath.Join(dir, secretbox.KeyFileName))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("secret.key after unwrap: %v", err)
	}
	journal, _ := keywrap.ReadJournal(dir)
	if len(journal) != 1 || journal[0].Action != keywrap.ActionUnwrappedToFile || !strings.HasPrefix(journal[0].Actor, "local") {
		t.Fatalf("journal = %+v", journal)
	}
	if code, _, errOut = runKeyCmd(nil, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 1 || !strings.Contains(errOut, "no secret.key.wrapped") {
		t.Fatalf("second unwrap = %d %q", code, errOut)
	}
}

func TestKeyRewrap(t *testing.T) {
	dir, key, fakes := keyTestDir(t)
	if code, _, errOut := runKeyCmd(nil, "rewrap", "--data-dir", dir); code != 2 || !strings.Contains(errOut, "--to") {
		t.Fatalf("rewrap without a target = %d %q", code, errOut)
	}
	to := "vault-transit:" + keyTestNew
	code, out, errOut := runKeyCmd(map[string]string{"MONGORESCUE_KEY_WRAP": to}, "rewrap", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "Set MONGORESCUE_KEY_WRAP="+to) {
		t.Fatalf("rewrap = %d %q %q", code, out, errOut)
	}
	codec := keywrap.NewCodec(keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: keyTestNew}, fakes[keyTestNew])
	got, err := codec.ReadKey(context.Background(), filepath.Join(dir, keywrap.WrappedKeyFileName))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("secret.key.wrapped after rewrap: %v", err)
	}
	code, out, _ = runKeyCmd(nil, "rewrap", "--data-dir", dir, "--to", to)
	if code != 0 || !strings.Contains(out, "nothing changed") {
		t.Fatalf("second rewrap = %d %q", code, out)
	}
	fakes[keyTestNew].SetDown(-1)
	if code, _, errOut = runKeyCmd(nil, "rewrap", "--data-dir", dir, "--to", "aws-kms:"+keyTestOld); code != 1 || !strings.Contains(errOut, "unreachable") {
		t.Fatalf("rewrap with the KMS down = %d %q", code, errOut)
	}
}

func TestKeyWrapFlag(t *testing.T) {
	var stderr bytes.Buffer
	cfg, _, _, err := parseFlags([]string{"-key-wrap", "gcp-kms:projects/p/locations/l/keyRings/r/cryptoKeys/k"}, func(string) string { return "" }, &stderr)
	if err != nil || cfg.KeyWrap.Provider != keywrap.ProviderGCPKMS {
		t.Fatalf("parseFlags = %+v, %v", cfg, err)
	}
	if _, _, _, err = parseFlags([]string{"-key-wrap", "gcp-kms:nope"}, func(string) string { return "" }, &stderr); err == nil ||
		!strings.Contains(err.Error(), "-key-wrap") {
		t.Fatalf("invalid -key-wrap = %v", err)
	}
}
