package operations_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// sealWith encrypts plain with the current encryptor of env.
func sealWith(t *testing.T, env *protEnv, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := env.settings.Encryptor().Encrypt(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, plain)
	_ = w.Close()
	return buf.Bytes()
}

func openWith(t *testing.T, env *protEnv, sealed []byte) string {
	t.Helper()
	plain, err := env.settings.Decryptor().Decrypt(bytes.NewReader(sealed))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	b, _ := io.ReadAll(plain)
	return string(b)
}

func TestRotateEncryptionKeyKeepsOldBackupsRestorable(t *testing.T) {
	env := newProtEnv(t)
	identity, recipient, _ := encryption.GenerateX25519()
	_, escrow, _ := encryption.GenerateX25519()
	on, recipients := true, []string{recipient, escrow}
	if _, err := env.settings.Update(context.Background(), settings.Patch{Encryption: &settings.EncryptionPatch{
		Enabled: &on, Identity: &identity, Recipients: &recipients}}); err != nil {
		t.Fatal(err)
	}
	before := sealWith(t, env, "old backup")

	res, err := env.svc.RotateEncryptionKey(asUser("alice", auth.ScopeAdmin), operations.EncryptionRotationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	enc := env.settings.Current().Encryption
	if res.OldFingerprint != recipient || res.NewFingerprint == recipient || enc.Identity == identity {
		t.Fatalf("rotation = %+v", res)
	}
	// The new public key replaces the old identity's; other recipients stay.
	if slices.Contains(enc.Recipients, recipient) || !slices.Contains(enc.Recipients, escrow) || !slices.Contains(enc.Recipients, res.NewFingerprint) {
		t.Fatalf("recipients = %v", enc.Recipients)
	}
	if len(enc.RetiredKeys) != 1 || enc.RetiredKeys[0].Kind != settings.KindX25519 {
		t.Fatalf("retired keys = %+v", enc.RetiredKeys)
	}
	if got := openWith(t, env, before); got != "old backup" {
		t.Fatalf("old backup = %q", got)
	}
	if got := openWith(t, env, sealWith(t, env, "new backup")); got != "new backup" {
		t.Fatalf("new backup = %q", got)
	}
	found := slices.ContainsFunc(env.pub.events, func(e events.Event) bool {
		return e.Type == events.SecurityKeyRotated && e.Action == operations.KeyKindEncryption
	})
	if !found {
		t.Fatal("no security.key_rotated event")
	}
}

func TestRotateEncryptionPassphrase(t *testing.T) {
	env := newProtEnv(t)
	on, mode, pass := true, settings.ModePassphrase, "the first long passphrase"
	if _, err := env.settings.Update(context.Background(), settings.Patch{Encryption: &settings.EncryptionPatch{
		Enabled: &on, Mode: &mode, Passphrase: &pass}}); err != nil {
		t.Fatal(err)
	}
	before := sealWith(t, env, "old backup")
	ctx := asUser("alice", auth.ScopeAdmin)
	if _, err := env.svc.RotateEncryptionKey(ctx, operations.EncryptionRotationRequest{}); err == nil {
		t.Fatal("rotating a passphrase without a new one must fail")
	}
	if _, err := env.svc.RotateEncryptionKey(ctx, operations.EncryptionRotationRequest{Passphrase: "a brand new long passphrase"}); err != nil {
		t.Fatal(err)
	}
	enc := env.settings.Current().Encryption
	if enc.Passphrase != "a brand new long passphrase" || len(enc.RetiredKeys) != 1 || enc.RetiredKeys[0].Kind != settings.KindPassphrase {
		t.Fatalf("encryption = %+v", enc.RetiredKeys)
	}
	if got := openWith(t, env, before); got != "old backup" {
		t.Fatalf("old backup = %q", got)
	}
}

func TestRotateEncryptionKeyNeedsAdmin(t *testing.T) {
	env := newProtEnv(t)
	if _, err := env.svc.RotateEncryptionKey(asUser("bob", auth.ScopeOperator), operations.EncryptionRotationRequest{}); err == nil {
		t.Fatal("an operator rotated the encryption key")
	}
}
