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
	if _, err := restarted.AuthenticateAPIKey(ctx, importedKey+"x"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("wrong key = %v; want ErrUnauthenticated", err)
	}
	if created, err := restarted.ImportAPIKey(ctx, importedKey); err != nil || created {
		t.Fatalf("re-import = %v, %v; want a no-op", created, err)
	}
}
