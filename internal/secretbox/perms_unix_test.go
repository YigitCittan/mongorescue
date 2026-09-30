//go:build unix

package secretbox

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestGeneratedKeyFileIsPrivateWhateverTheUmask checks that a generated secret.key
// is 0600 even with a permissive umask. The umask is process-wide, so this test must
// not run in parallel.
func TestGeneratedKeyFileIsPrivateWhateverTheUmask(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	file := filepath.Join(t.TempDir(), "data", KeyFileName)
	got, err := LoadKey(KeySource{File: file}, slog.New(slog.DiscardHandler))
	if err != nil || !got.Created {
		t.Fatalf("LoadKey = %+v, %v", got, err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret.key mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}
