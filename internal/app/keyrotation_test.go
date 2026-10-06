package app

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// adminSession is a signed-in administrator.
func adminSession() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeAdmin,
		User: &auth.User{ID: "usr_admin", Username: "admin", Role: auth.RoleAdmin}})
}

// TestAppRotatesTheSecretKeyAndRestarts rotates secret.key in a running app,
// restarts it with the new key and checks that the old key is refused.
func TestAppRotatesTheSecretKeyAndRestarts(t *testing.T) {
	cfg := testConfig(t)
	keyFile := filepath.Join(cfg.DataDir, secretbox.KeyFileName)
	first, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := secretbox.ReadKeyFile(keyFile)
	res, err := first.ops.RotateSecretKey(adminSession())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	newKey, _ := secretbox.ReadKeyFile(keyFile)
	if bytes.Equal(newKey, oldKey) {
		t.Fatal("secret.key was not replaced")
	}
	if fp, _ := secretbox.Fingerprint(newKey); fp != res.NewFingerprint {
		t.Fatal("secret.key is not the rotated key")
	}
	again, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("restart with the rotated key: %v", err)
	}
	if err = again.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SecretKey = secretbox.EncodeKey(oldKey)
	if _, err = New(cfg, nil, WithGetenv(noEnv)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("start with the old key = %v; want ErrSecretKeyMismatch", err)
	}
}

func TestAppRefusesToRotateAnEnvKey(t *testing.T) {
	cfg := testConfig(t)
	key, _ := secretbox.GenerateKey()
	cfg.SecretKey = secretbox.EncodeKey(key)
	a, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err = a.ops.RotateSecretKey(adminSession()); !errors.Is(err, keyrotation.ErrEnvKey) {
		t.Fatalf("rotate = %v; want ErrEnvKey", err)
	}
}
