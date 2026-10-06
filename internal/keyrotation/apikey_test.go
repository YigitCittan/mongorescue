package keyrotation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// importedKey is a user-chosen key like a MONGORESCUE_API_KEY value.
const importedKey = "correct-horse-battery-staple"

func authOn(t *testing.T, st *store.SQLiteStore, key []byte) *auth.Service {
	t.Helper()
	sub, err := secretbox.DeriveSubkey(key, auth.ImportedKeySubkeyPurpose)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(st, auth.WithImportedKeySecret(sub))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestImportedAPIKeysVerifyAfterRotation imports a key MACed with a subkey of the
// old secret.key, rotates and restarts: the key verifies through the retired MAC
// key, is re-hashed under the new one on use, and is not imported twice.
func TestImportedAPIKeysVerifyAfterRotation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	opened := e.start(t)
	svc := authOn(t, opened.Store, opened.Key)
	if created, err := svc.ImportAPIKey(ctx, importedKey); err != nil || !created {
		t.Fatalf("ImportAPIKey = %v, %v", created, err)
	}
	if _, err := rotator(e, opened, nil, nil).Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := opened.Store.ListAPIKeys(ctx)
	_ = opened.Store.Close()

	again := e.start(t)
	restarted := authOn(t, again.Store, again.Key)
	if _, err := restarted.AuthenticateAPIKey(ctx, importedKey); err != nil {
		t.Fatalf("authenticate after rotation: %v", err)
	}
	after, _ := again.Store.ListAPIKeys(ctx)
	if len(after) != 1 || after[0].Prefix == before[0].Prefix || after[0].Hash == before[0].Hash {
		t.Fatal("imported key was not re-hashed under the new key")
	}
	if _, err := authOn(t, again.Store, again.Key).AuthenticateAPIKey(ctx, importedKey); err != nil {
		t.Fatalf("authenticate after re-hash: %v", err)
	}
	// The hash names its MAC key, so the retired one is dropped once nothing uses it.
	sub, _ := secretbox.DeriveSubkey(again.Key, auth.ImportedKeySubkeyPurpose)
	if kid, _ := auth.ImportedKeyHashKID(after[0].Hash); kid != auth.ImportedKeyMACID(sub) {
		t.Fatalf("hash key ID = %q; want the current MAC key's", kid)
	}
	if macs, err := again.Store.RetiredImportedKeyMACs(ctx); err != nil || len(macs) != 0 {
		t.Fatalf("retired MAC keys after the re-hash = %d, %v; want none", len(macs), err)
	}
	if _, err := restarted.AuthenticateAPIKey(ctx, importedKey+"x"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("wrong key = %v; want ErrUnauthenticated", err)
	}
	if created, err := restarted.ImportAPIKey(ctx, importedKey); err != nil || created {
		t.Fatalf("re-import = %v, %v; want a no-op", created, err)
	}
}

// TestReimportAfterRotationRehashes checks the import path on its own: a key MACed
// under the retired secret key is recognised by the next start's import (the
// variable is still set), re-hashed as "hmac-sha256:<kid>:" under the new MAC key
// and not imported twice.
func TestReimportAfterRotationRehashes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	opened := e.start(t)
	if created, err := authOn(t, opened.Store, opened.Key).ImportAPIKey(ctx, importedKey); err != nil || !created {
		t.Fatalf("ImportAPIKey = %v, %v", created, err)
	}
	if _, err := rotator(e, opened, nil, nil).Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := opened.Store.ListAPIKeys(ctx)
	_ = opened.Store.Close()

	again := e.start(t)
	restarted := authOn(t, again.Store, again.Key)
	if created, err := restarted.ImportAPIKey(ctx, importedKey); err != nil || created {
		t.Fatalf("re-import after rotation = %v, %v; want a no-op", created, err)
	}
	after, _ := again.Store.ListAPIKeys(ctx)
	if len(after) != 1 || after[0].ID != before[0].ID || after[0].Hash == before[0].Hash {
		t.Fatalf("keys after re-import = %+v; want the one record, re-hashed", after)
	}
	sub, _ := secretbox.DeriveSubkey(again.Key, auth.ImportedKeySubkeyPurpose)
	if kid, ok := auth.ImportedKeyHashKID(after[0].Hash); !ok || kid != auth.ImportedKeyMACID(sub) {
		t.Fatalf("hash key ID = %q, %v; want the current MAC key's", kid, ok)
	}
	if _, err := restarted.AuthenticateAPIKey(ctx, importedKey); err != nil {
		t.Fatalf("authenticate after the re-import: %v", err)
	}
}
