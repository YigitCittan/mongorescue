package auth_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// importedKey is a user-chosen static key like a MONGORESCUE_API_KEY value.
const importedKey = "correct-horse-battery-staple"

// serviceOn returns a Service on st with the imported-key secret.
func serviceOn(t *testing.T, st *store.SQLiteStore, secret []byte) *auth.Service {
	t.Helper()
	svc, err := auth.NewService(st, auth.WithBcryptCost(bcrypt.MinCost), auth.WithImportedKeySecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestImportedKeyIsStoredAsKeyedMAC checks that a user-chosen imported key is not
// stored as a plain SHA-256 digest (which a copy of the database alone would let an
// attacker guess offline) but as an HMAC under the server secret, and that it keeps
// verifying across restarts with the same secret only.
func TestImportedKeyIsStoredAsKeyedMAC(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	secret := bytes.Repeat([]byte{7}, 32)
	svc := serviceOn(t, f.store, secret)
	if created, err := svc.ImportAPIKey(ctx, importedKey); err != nil || !created {
		t.Fatalf("ImportAPIKey = %v, %v", created, err)
	}
	p, err := svc.AuthenticateAPIKey(ctx, importedKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey = %v", err)
	}
	keys, err := f.store.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].ID != p.APIKeyID {
		t.Fatalf("ListAPIKeys = %v, %v", keys, err)
	}
	stored := keys[0]
	if !strings.HasPrefix(stored.Hash, "hmac-sha256:") || stored.Hash == auth.HashToken(importedKey) ||
		strings.Contains(stored.Hash, auth.HashToken(importedKey)) || strings.Contains(stored.Hash, importedKey) {
		t.Fatalf("stored hash = %q; want an HMAC, not the SHA-256 of the key", stored.Hash)
	}
	if !strings.HasPrefix(stored.Prefix, "K") || len(stored.Prefix) != 8 {
		t.Fatalf("stored prefix = %q; want K + 7 characters", stored.Prefix)
	}

	// A restart with the same secret (derived from secret.key) verifies the key.
	if _, err := serviceOn(t, f.store, secret).AuthenticateAPIKey(ctx, importedKey); err != nil {
		t.Fatalf("same secret after restart: %v", err)
	}
	// Another secret neither finds nor verifies it.
	other := serviceOn(t, f.store, bytes.Repeat([]byte{8}, 32))
	if _, err := other.AuthenticateAPIKey(ctx, importedKey); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("other secret: %v; want ErrUnauthenticated", err)
	}
	// Importing the same key again is still a no-op.
	if created, err := svc.ImportAPIKey(ctx, importedKey); err != nil || created {
		t.Fatalf("second ImportAPIKey = %v, %v; want a no-op", created, err)
	}
}

// TestLegacyImportedKeyKeepsVerifying checks the dual check: a key imported by an
// earlier release (prefix "L" + SHA-256, plain SHA-256 hash) still authenticates, and
// a wrong key with the same length does not.
func TestLegacyImportedKeyKeepsVerifying(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	legacy := &auth.APIKey{
		ID: "key_legacy0000000000", Name: "Imported from MONGORESCUE_API_KEY",
		Prefix: "L" + auth.HashToken(importedKey)[:7], Scope: auth.ScopeAdmin,
		Hash: auth.HashToken(importedKey), CreatedAt: time.Now().UTC(),
	}
	if err := f.store.CreateAPIKey(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	svc := serviceOn(t, f.store, bytes.Repeat([]byte{7}, 32))
	p, err := svc.AuthenticateAPIKey(ctx, importedKey)
	if err != nil || p.APIKeyID != legacy.ID || p.Scope != auth.ScopeAdmin {
		t.Fatalf("legacy imported key = %+v, %v", p, err)
	}
	wrong := strings.Repeat("x", len(importedKey))
	if _, err := svc.AuthenticateAPIKey(ctx, wrong); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("wrong key: %v; want ErrUnauthenticated", err)
	}
}

// TestGeneratedAndImportedFormatKey checks that an imported key in the generated
// "mr_" format is looked up by its own prefix and verified against its MAC.
func TestGeneratedAndImportedFormatKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	key := "mr_abcdefgh_" + strings.Repeat("a", 32)
	svc := serviceOn(t, f.store, bytes.Repeat([]byte{7}, 32))
	if created, err := svc.ImportAPIKey(ctx, key); err != nil || !created {
		t.Fatalf("ImportAPIKey = %v, %v", created, err)
	}
	stored, err := f.store.GetAPIKeyByPrefix(ctx, "abcdefgh")
	if err != nil || !strings.HasPrefix(stored.Hash, "hmac-sha256:") {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if _, err := svc.AuthenticateAPIKey(ctx, key); err != nil {
		t.Fatalf("AuthenticateAPIKey = %v", err)
	}
	tampered := key[:len(key)-1] + "b"
	if _, err := svc.AuthenticateAPIKey(ctx, tampered); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("tampered key: %v; want ErrUnauthenticated", err)
	}
}
