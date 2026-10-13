package keywrap_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// loadLogged is load with the log captured.
func loadLogged(dir string, w keywrap.Wrapper) (*keywrap.Loaded, string, error) {
	var buf bytes.Buffer
	loaded, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: testSpec, Wrapper: w, Retry: fastRetry,
		Sleep: keywraptest.NoSleep, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	return loaded, buf.String(), err
}

// listDir returns the names in dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestMigrationLeavesNoPlaintextUnderAnyName(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
	if _, err := load(dir, keywraptest.New("kms")); err != nil {
		t.Fatal(err)
	}
	for _, name := range listDir(t, dir) {
		if strings.HasPrefix(name, secretbox.TempKeyFilePrefix) {
			t.Errorf("temporary file %s left behind", name)
		}
	}
	assertNoPlaintext(t, dir, key)
}

// TestStartAfterACrashDuringSecureRemoval covers the states a crash during the
// removal of a plaintext key file can leave next to a valid wrapped key: the key
// moved aside under a temporary name, or a half-overwritten (unreadable) file
// under its own name. Each start succeeds, removes the leftover and warns.
func TestStartAfterACrashDuringSecureRemoval(t *testing.T) {
	kms := keywraptest.New("kms")
	setup := func(t *testing.T) (string, []byte) {
		t.Helper()
		dir := t.TempDir()
		key := writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
		writeKey(t, filepath.Join(dir, secretbox.PreviousKeyFileName))
		if _, err := load(dir, kms); err != nil {
			t.Fatal(err)
		}
		return dir, key
	}

	t.Run("moved aside", func(t *testing.T) {
		dir, key := setup(t)
		leftover := filepath.Join(dir, secretbox.TempKeyFilePrefix+"rm-0123456789abcdef")
		if err := secretbox.WriteKeyFile(leftover, key); err != nil {
			t.Fatal(err)
		}
		got, log, err := loadLogged(dir, kms)
		if err != nil || !bytes.Equal(got.Key, key) {
			t.Fatalf("start with a moved-aside key = %v", err)
		}
		if _, err = os.Stat(leftover); !errors.Is(err, os.ErrNotExist) || !strings.Contains(log, "temporary key file left by a crash") {
			t.Fatalf("leftover kept (%v) or not reported: %s", err, log)
		}
		assertNoPlaintext(t, dir, key)
	})
	for _, name := range []string{secretbox.KeyFileName, secretbox.PreviousKeyFileName} {
		t.Run("half overwritten "+name, func(t *testing.T) {
			dir, key := setup(t)
			plain := filepath.Join(dir, name)
			if err := os.WriteFile(plain, make([]byte, 44), 0o600); err != nil { // zeros: no key
				t.Fatal(err)
			}
			got, log, err := loadLogged(dir, kms)
			if err != nil || !bytes.Equal(got.Key, key) {
				t.Fatalf("start with an unreadable %s = %v", name, err)
			}
			if _, err = os.Stat(plain); !errors.Is(err, os.ErrNotExist) || !strings.Contains(log, "unreadable plaintext key file") {
				t.Fatalf("%s kept (%v) or not reported: %s", name, err, log)
			}
		})
	}

	t.Run("unreadable without a wrapped copy", func(t *testing.T) {
		// Without a verified wrapped counterpart nothing is removed: the start fails.
		dir := t.TempDir()
		plain := filepath.Join(dir, secretbox.KeyFileName)
		if err := os.WriteFile(plain, make([]byte, 44), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := load(dir, kms); err == nil {
			t.Fatal("an unreadable secret.key without a wrapped copy was accepted")
		}
		if _, err := os.Stat(plain); err != nil {
			t.Fatalf("the unreadable secret.key was removed: %v", err)
		}
	})
	t.Run("another key is still a conflict", func(t *testing.T) {
		dir, _ := setup(t)
		writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
		if _, err := load(dir, kms); !errors.Is(err, keywrap.ErrConflict) {
			t.Fatalf("start with a different readable key = %v; want ErrConflict", err)
		}
	})
}
