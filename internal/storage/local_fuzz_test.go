package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLocalKey checks that a key the local driver accepts never resolves outside its
// base directory (nor to the base directory itself), and that saving, reading and
// deleting an object under that key writes nothing outside it.
func FuzzLocalKey(f *testing.F) {
	for _, k := range []string{
		"backups/db/2026.archive", "a", "a/b/c", "../x", "a/../../x", "a/../b", "/etc/passwd", `\etc\passwd`,
		`..\x`, "C:x", "C:/x", ".", "./", " ", " ../x", "a/./b", "a//b", "..", "...", "a/..", "NUL", "COM1.txt",
		"a\x00b", strings.Repeat("a/", 50) + "x", "~/x", "$HOME/x", "a/b/", "a.tmp.1",
	} {
		f.Add(k)
	}
	f.Fuzz(func(t *testing.T, key string) {
		root := t.TempDir()
		base := filepath.Join(root, "base")
		s, err := NewLocalStorage(base)
		if err != nil {
			t.Fatal(err)
		}
		full, err := s.resolvePath(key)
		if err != nil {
			if !errors.Is(err, ErrInvalidKey) && !errors.Is(err, ErrPathTraversal) {
				t.Fatalf("resolvePath(%q): unexpected error %v", key, err)
			}
			return
		}
		rel, err := filepath.Rel(s.baseDir, full)
		if err != nil || rel == "." || !filepath.IsLocal(rel) || !strings.HasPrefix(full, s.baseDir+string(filepath.Separator)) {
			t.Fatalf("resolvePath(%q) = %q escapes %q", key, full, s.baseDir)
		}

		ctx := context.Background()
		if _, saveErr := s.Save(ctx, key, strings.NewReader("data")); saveErr == nil {
			if _, statErr := s.Stat(ctx, key); statErr != nil {
				t.Fatalf("Stat(%q) after Save: %v", key, statErr)
			}
			if delErr := s.Delete(ctx, key); delErr != nil {
				t.Fatalf("Delete(%q) after Save: %v", key, delErr)
			}
		}
		err = filepath.WalkDir(root, func(p string, _ fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p != root && p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
				t.Errorf("key %q wrote %s outside the base directory", key, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(base); err != nil {
			t.Fatalf("base directory gone after key %q: %v", key, err)
		}
	})
}
