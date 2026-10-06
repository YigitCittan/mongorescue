package keyrotation_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStartupRemovesStaleTempKeyFiles leaves the temporary file of an interrupted
// key write behind and checks that the next start removes it and nothing else.
func TestStartupRemovesStaleTempKeyFiles(t *testing.T) {
	e := newEnv(t)
	opened := e.start(t)
	_ = opened.Store.Close()
	stale := filepath.Join(e.dir, ".secret.key.tmp-123456")
	other := filepath.Join(e.dir, "notes.txt")
	for _, p := range []string{stale, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.start(t)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temporary key file: %v; want it removed", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated file: %v; want it kept", err)
	}
	if !exists(t, e.files.Current) {
		t.Fatal("secret.key was removed")
	}
}
