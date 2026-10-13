package keywrap_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

var otherSpec = keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: "https://vault:8200/transit/keys/next"}

// wrappedDir returns a data directory whose secret.key and secret.key.previous
// are wrapped with kms, and their keys.
func wrappedDir(t *testing.T, kms keywrap.Wrapper) (dir string, cur, prev []byte) {
	t.Helper()
	dir = t.TempDir()
	cur = writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
	prev = writeKey(t, filepath.Join(dir, secretbox.PreviousKeyFileName))
	if _, err := load(dir, kms); err != nil {
		t.Fatal(err)
	}
	if err := keywrap.ClearJournal(dir); err != nil {
		t.Fatal(err)
	}
	return dir, cur, prev
}

func offline(dir string, fakes map[string]*keywraptest.Fake) keywrap.OfflineConfig {
	return keywrap.OfflineConfig{DataDir: dir, Open: keywraptest.Opener(fakes), Actor: "local:alice", Retry: fastRetry,
		Now: func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }}
}

func TestUnwrapToFiles(t *testing.T) {
	kms := keywraptest.New("kms")
	dir, cur, prev := wrappedDir(t, kms)
	fakes := map[string]*keywraptest.Fake{testSpec.KeyURI: kms}

	change, err := keywrap.UnwrapToFiles(context.Background(), offline(dir, fakes))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{secretbox.KeyFileName: cur, secretbox.PreviousKeyFileName: prev} {
		got, err := secretbox.ReadKeyFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s after unwrap: %v", name, err)
		}
		if exists(t, filepath.Join(dir, name+keywrap.Suffix)) {
			t.Errorf("%s.wrapped was kept", name)
		}
	}
	fp, _ := secretbox.Fingerprint(cur)
	if change.Action != keywrap.ActionUnwrappedToFile || change.Actor != "local:alice" || change.Fingerprint != fp ||
		change.PreviousKeyURI != testSpec.KeyURI || len(change.Files) != 2 || change.Files[1] != secretbox.KeyFileName {
		t.Errorf("change = %+v", change)
	}
	if !strings.Contains(change.Detail(), "no longer protected") {
		t.Errorf("Detail = %q", change.Detail())
	}
	journal, err := keywrap.ReadJournal(dir)
	if err != nil || len(journal) != 1 || journal[0].Action != keywrap.ActionUnwrappedToFile {
		t.Fatalf("journal = %+v, %v", journal, err)
	}
	if _, err = keywrap.UnwrapToFiles(context.Background(), offline(dir, fakes)); !errors.Is(err, keywrap.ErrNotWrapped) {
		t.Fatalf("second unwrap = %v; want ErrNotWrapped", err)
	}
}

func TestUnwrapToFilesResumesAndRefusesConflicts(t *testing.T) {
	kms := keywraptest.New("kms")
	dir, cur, _ := wrappedDir(t, kms)
	fakes := map[string]*keywraptest.Fake{testSpec.KeyURI: kms}
	plain := filepath.Join(dir, secretbox.KeyFileName)

	// An interrupted run left the same key in plain form: it is kept.
	if err := secretbox.WriteKeyFile(plain, cur); err != nil {
		t.Fatal(err)
	}
	kms.SetDown(-1)
	if _, err := keywrap.UnwrapToFiles(context.Background(), offline(dir, fakes)); !errors.Is(err, keywrap.ErrUnavailable) {
		t.Fatalf("unwrap with the KMS down = %v", err)
	}
	if !exists(t, plain+keywrap.Suffix) {
		t.Fatal("a failed unwrap removed secret.key.wrapped")
	}
	kms.SetDown(0)
	if _, err := keywrap.UnwrapToFiles(context.Background(), offline(dir, fakes)); err != nil {
		t.Fatalf("resumed unwrap = %v", err)
	}

	dir, _, _ = wrappedDir(t, kms)
	writeKey(t, filepath.Join(dir, secretbox.KeyFileName))
	if _, err := keywrap.UnwrapToFiles(context.Background(), offline(dir, fakes)); !errors.Is(err, keywrap.ErrConflict) {
		t.Fatalf("unwrap over another plaintext key = %v; want ErrConflict", err)
	}
}

func TestRewrap(t *testing.T) {
	oldKMS, newKMS := keywraptest.New("old"), keywraptest.New("new")
	dir, cur, prev := wrappedDir(t, oldKMS)
	fakes := map[string]*keywraptest.Fake{testSpec.KeyURI: oldKMS, otherSpec.KeyURI: newKMS}

	// The new KMS key failing leaves every file as it was.
	newKMS.SetDown(-1)
	if _, err := keywrap.Rewrap(context.Background(), offline(dir, fakes), otherSpec); !errors.Is(err, keywrap.ErrUnavailable) {
		t.Fatalf("rewrap with the new KMS down = %v", err)
	}
	if f, err := keywrap.ReadFile(filepath.Join(dir, keywrap.WrappedKeyFileName)); err != nil || f.Spec() != testSpec {
		t.Fatalf("secret.key.wrapped changed: %+v, %v", f, err)
	}
	newKMS.SetDown(0)

	change, err := keywrap.Rewrap(context.Background(), offline(dir, fakes), otherSpec)
	if err != nil {
		t.Fatal(err)
	}
	if change.Action != keywrap.ActionRewrapped || change.KeyURI != otherSpec.KeyURI || change.PreviousKeyURI != testSpec.KeyURI ||
		change.KeyID != newKMS.KeyID() || len(change.Files) != 2 {
		t.Errorf("change = %+v", change)
	}
	if !strings.Contains(change.Detail(), "rewrapped from aws-kms:alias/mongorescue to vault-transit:") {
		t.Errorf("Detail = %q", change.Detail())
	}
	codec := keywrap.NewCodec(otherSpec, newKMS)
	for name, want := range map[string][]byte{keywrap.WrappedKeyFileName: cur, secretbox.PreviousKeyFileName + keywrap.Suffix: prev} {
		got, err := codec.ReadKey(context.Background(), filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s after rewrap: %v", name, err)
		}
	}
	// The old KMS key no longer opens anything.
	if _, err = load(dir, oldKMS); !errors.Is(err, keywrap.ErrKeyMismatch) {
		t.Fatalf("Load with the old spec = %v; want ErrKeyMismatch", err)
	}
	assertNoPlaintext(t, dir, cur)

	again, err := keywrap.Rewrap(context.Background(), offline(dir, fakes), otherSpec)
	if err != nil || len(again.Files) != 0 {
		t.Fatalf("rewrap to the same key = %+v, %v", again, err)
	}
	journal, _ := keywrap.ReadJournal(dir)
	if len(journal) != 1 {
		t.Errorf("journal = %+v; want the one rewrap", journal)
	}
	if _, err = keywrap.Rewrap(context.Background(), offline(dir, fakes), keywrap.Spec{}); !errors.Is(err, keywrap.ErrInvalidSpec) {
		t.Errorf("rewrap without a target = %v", err)
	}
	if _, err = keywrap.Rewrap(context.Background(), offline(t.TempDir(), fakes), otherSpec); !errors.Is(err, keywrap.ErrNotWrapped) {
		t.Errorf("rewrap of a plain directory = %v", err)
	}
	if _, err = keywrap.Rewrap(context.Background(), keywrap.OfflineConfig{}, otherSpec); err == nil {
		t.Error("rewrap without a data directory succeeded")
	}
}

func TestJournal(t *testing.T) {
	dir := t.TempDir()
	if changes, err := keywrap.ReadJournal(dir); err != nil || changes != nil {
		t.Fatalf("empty journal = %v, %v", changes, err)
	}
	for _, a := range []string{keywrap.ActionWrapped, keywrap.ActionRewrapped} {
		if err := keywrap.AppendJournal(dir, keywrap.Change{Action: a, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// A torn last line is skipped.
	f, err := os.OpenFile(filepath.Join(dir, keywrap.JournalFileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"action":"unwr`)
	_ = f.Close()
	changes, err := keywrap.ReadJournal(dir)
	if err != nil || len(changes) != 2 || changes[1].Action != keywrap.ActionRewrapped {
		t.Fatalf("journal = %+v, %v", changes, err)
	}
	if err = keywrap.ClearJournal(dir); err != nil {
		t.Fatal(err)
	}
	if changes, _ = keywrap.ReadJournal(dir); len(changes) != 0 {
		t.Fatal("journal not cleared")
	}
	if d := (keywrap.Change{Action: "other"}).Detail(); d != "other" {
		t.Errorf("Detail = %q", d)
	}
}
