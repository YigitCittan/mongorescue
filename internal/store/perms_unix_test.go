//go:build unix

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestDatabaseFilePermissions checks that the database and its WAL files are 0600
// regardless of the umask, and that a looser pre-existing file is tightened. The
// umask is process-wide, so this test must not run in parallel.
func TestDatabaseFilePermissions(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := t.TempDir()
	loose := filepath.Join(dir, "loose.db")
	if err := os.WriteFile(loose, nil, 0o644); err != nil { //nolint:gosec // G306: deliberately loose, the store must tighten it.
		t.Fatal(err)
	}

	for _, path := range []string{filepath.Join(dir, dbFile), loose} {
		s := storetest.Open(t, path)
		if err := s.SaveJob(context.Background(), &models.Job{ID: "j", Name: "J"}); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s mode = %o; want 600 (it stores notification secrets)", filepath.Base(p), perm)
			}
		}
	}
}
