package keyrotation_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/keyrotation"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// TestCommitErrorAfterTheWriteKeepsTheNewKey makes the commit report an error
// although the data was written (an fsync error after the WAL frame). The rotation
// must notice that the database uses the new key and install it, never discarding
// secret.key.next; a restart then opens the database.
func TestCommitErrorAfterTheWriteKeepsTheNewKey(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	seed(t, opened.Store)
	oldKey := opened.Key
	r := keyrotation.New(keyrotation.Config{
		Files: e.files, Store: opened.Store, Key: opened.Key,
		RetiredMAC:  func(old []byte) ([]byte, error) { return secretbox.DeriveSubkey(old, auth.ImportedKeySubkeyPurpose) },
		CommitError: func() error { return errors.New("disk I/O error (fsync)") },
		Logger:      slog.New(slog.DiscardHandler),
	})
	if _, err := r.Rotate(context.Background()); err != nil {
		t.Fatalf("rotate = %v; want the committed rotation to complete", err)
	}
	cur := readKey(t, e.files.Current)
	if bytes.Equal(cur, oldKey) || exists(t, e.files.Next) {
		t.Fatal("the new key was not installed")
	}
	checkSecrets(t, opened.Store)
	_ = opened.Store.Close()
	again := e.start(t)
	if !bytes.Equal(again.Key, cur) {
		t.Fatal("the restart does not use the new key")
	}
	checkSecrets(t, again.Store)
}
