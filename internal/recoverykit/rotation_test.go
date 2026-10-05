package recoverykit_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/metabackup"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// TestRotatedKeyChangesTheKit checks that a rotated secret.key changes the
// fingerprint (the reminder fires again), the key and the metadata prefix of the
// next kit, and that the previous key is only carried when asked for.
func TestRotatedKeyChangesTheKit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before, err := f.svc.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := secretbox.GenerateKey()
	const newPrefix = metabackup.Prefix + "fedcba9876543210/"
	if err = f.svc.SetSecretKey(next, newPrefix); err != nil {
		t.Fatal(err)
	}
	after, _ := f.svc.Fingerprint(ctx)
	if after == before {
		t.Fatal("the fingerprint did not change with the key")
	}
	files := sealAndOpen(t, f)
	if strings.TrimSpace(files[recoverykit.SecretKeyName]) != secretbox.EncodeKey(next) {
		t.Fatal("the kit does not carry the rotated key")
	}
	if !strings.Contains(files[recoverykit.ManifestName], newPrefix) {
		t.Fatal("the kit does not carry the new metadata prefix")
	}
	if _, ok := files[recoverykit.PreviousKeyName]; ok {
		t.Fatal("the previous key was included without being asked for")
	}
	if _, err = f.svc.PrepareWith(ctx, recoverykit.PrepareOptions{IncludePreviousKey: true}); !errors.Is(err, recoverykit.ErrNoPreviousKey) {
		t.Fatalf("PrepareWith without a previous key file = %v; want ErrNoPreviousKey", err)
	}
}

func TestKitCarriesThePreviousKeyOnRequest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	prevFile := filepath.Join(t.TempDir(), secretbox.PreviousKeyFileName)
	if err := secretbox.WriteKeyFile(prevFile, f.key); err != nil {
		t.Fatal(err)
	}
	svc, err := recoverykit.New(recoverykit.Config{SecretKey: f.key, Settings: f.settings, Targets: f.targets,
		PreviousKeyFile: prevFile, WorkFactor: 10})
	if err != nil {
		t.Fatal(err)
	}
	kit, err := svc.PrepareWith(ctx, recoverykit.PrepareOptions{IncludePreviousKey: true})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err = svc.Seal(&sealed, kit, passphrase); err != nil {
		t.Fatal(err)
	}
	files := open(t, sealed.Bytes(), passphrase)
	if strings.TrimSpace(files[recoverykit.PreviousKeyName]) != secretbox.EncodeKey(f.key) {
		t.Fatal("the kit does not carry secret.key.previous")
	}
	if !strings.Contains(files[recoverykit.ReadmeName], "secret.key.previous") ||
		!strings.Contains(files[recoverykit.ManifestName], `"previous_file"`) {
		t.Fatal("README or manifest does not describe the previous key")
	}
}
