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

// oldEnv configures the KMS key the test directory is wrapped with.
var oldEnv = map[string]string{"MONGORESCUE_KEY_WRAP": "aws-kms:" + keyTestOld}

func TestKeyUnwrapNeedsConfirmationAndAStoppedServer(t *testing.T) {
	dir, key, _ := keyTestDir(t)
	if code, _, errOut := runKeyCmd(oldEnv, "unwrap", "--data-dir", dir); code != 2 || !strings.Contains(errOut, "--confirm-plaintext") {
		t.Fatalf("unwrap without confirmation = %d %q", code, errOut)
	}
	lock, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runKeyCmd(oldEnv, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 1 || !strings.Contains(errOut, "stop it first") {
		t.Fatalf("unwrap while running = %d %q", code, errOut)
	}
	_ = lock.Release()

	code, out, errOut := runKeyCmd(oldEnv, "unwrap", "--data-dir", dir, "--confirm-plaintext")
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
	if code, _, errOut = runKeyCmd(oldEnv, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 1 || !strings.Contains(errOut, "no secret.key.wrapped") {
		t.Fatalf("second unwrap = %d %q", code, errOut)
	}
}

// TestKeyCommandsTakeTheKMSKeyFromTheConfiguration checks that the KMS key comes
// from MONGORESCUE_KEY_WRAP or a confirmed --from, never from the data directory.
func TestKeyCommandsTakeTheKMSKeyFromTheConfiguration(t *testing.T) {
	dir, key, _ := keyTestDir(t)
	var opened []string
	inner := openKeyWrapper
	openKeyWrapper = func(ctx context.Context, s keywrap.Spec) (keywrap.Wrapper, error) {
		opened = append(opened, s.KeyURI)
		return inner(ctx, s)
	}
	if code, _, errOut := runKeyCmd(nil, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 2 || !strings.Contains(errOut, "MONGORESCUE_KEY_WRAP") {
		t.Fatalf("unwrap without a configured key = %d %q", code, errOut)
	}
	other := map[string]string{"MONGORESCUE_KEY_WRAP": "vault-transit:" + keyTestNew}
	if code, _, errOut := runKeyCmd(other, "unwrap", "--data-dir", dir, "--confirm-plaintext"); code != 1 || !strings.Contains(errOut, "--from") {
		t.Fatalf("unwrap with another configured key = %d %q", code, errOut)
	}
	if code, _, errOut := runKeyCmd(other, "unwrap", "--data-dir", dir, "--confirm-plaintext", "--from", "aws-kms:"+keyTestOld); code != 2 ||
		!strings.Contains(errOut, "--confirm-from") {
		t.Fatalf("unconfirmed --from = %d %q", code, errOut)
	}
	if len(opened) != 0 {
		t.Fatalf("KMS opened for %v before the key was confirmed", opened)
	}
	code, _, errOut := runKeyCmd(other, "unwrap", "--data-dir", dir, "--confirm-plaintext", "--from", "aws-kms:"+keyTestOld, "--confirm-from")
	if code != 0 || len(opened) != 1 || opened[0] != keyTestOld {
		t.Fatalf("confirmed --from = %d %q, opened %v", code, errOut, opened)
	}
	if got, err := secretbox.ReadKeyFile(filepath.Join(dir, secretbox.KeyFileName)); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("secret.key after unwrap: %v", err)
	}
}

func TestKeyRewrap(t *testing.T) {
	dir, key, fakes := keyTestDir(t)
	to := "vault-transit:" + keyTestNew
	if code, _, errOut := runKeyCmd(oldEnv, "rewrap", "--data-dir", dir); code != 2 || !strings.Contains(errOut, "--to") {
		t.Fatalf("rewrap without a target = %d %q", code, errOut)
	}
	code, out, errOut := runKeyCmd(oldEnv, "rewrap", "--data-dir", dir, "--to", to)
	if code != 0 || !strings.Contains(out, "Set MONGORESCUE_KEY_WRAP="+to) {
		t.Fatalf("rewrap = %d %q %q", code, out, errOut)
	}
	codec := keywrap.NewCodec(keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: keyTestNew}, fakes[keyTestNew])
	got, err := codec.ReadKey(context.Background(), filepath.Join(dir, keywrap.WrappedKeyFileName))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("secret.key.wrapped after rewrap: %v", err)
	}
	code, out, _ = runKeyCmd(oldEnv, "rewrap", "--data-dir", dir, "--to", to)
	if code != 0 || !strings.Contains(out, "nothing changed") {
		t.Fatalf("second rewrap = %d %q", code, out)
	}
	fakes[keyTestNew].SetDown(-1)
	newEnv := map[string]string{"MONGORESCUE_KEY_WRAP": to}
	if code, _, errOut = runKeyCmd(newEnv, "rewrap", "--data-dir", dir, "--to", "aws-kms:"+keyTestOld); code != 1 || !strings.Contains(errOut, "unreachable") {
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
