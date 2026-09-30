package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// localOps runs every key-taking operation of the local driver.
func localOps(s *LocalStorage) map[string]func(ctx context.Context, key string) error {
	return map[string]func(ctx context.Context, key string) error{
		"Save": func(ctx context.Context, k string) error {
			_, err := s.Save(ctx, k, strings.NewReader("malicious"))
			return err
		},
		"Retrieve": func(ctx context.Context, k string) error {
			rc, err := s.Retrieve(ctx, k)
			if err == nil {
				_ = rc.Close()
			}
			return err
		},
		"Stat":   func(ctx context.Context, k string) error { _, err := s.Stat(ctx, k); return err },
		"Delete": func(ctx context.Context, k string) error { return s.Delete(ctx, k) },
	}
}

// TestLocalStorageRejectsHostileKeys feeds every operation keys that try to leave the
// storage root or to mean different files on different platforms, and checks that
// nothing is created, read or deleted outside (or inside) the root.
func TestLocalStorageRejectsHostileKeys(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, dir := newTestLocalStorage(t)
	ctx := context.Background()
	for _, tc := range []struct {
		key  string
		want error
	}{
		{"../victim.txt", ErrPathTraversal},
		{"../../../../../../../../etc/passwd", ErrPathTraversal},
		{"db/../../victim.txt", ErrPathTraversal},
		{"db/./../../victim.txt", ErrPathTraversal},
		{"..", ErrPathTraversal},
		{"../", ErrPathTraversal},
		{victim, ErrPathTraversal},
		{"/etc/passwd", ErrPathTraversal},
		{"//server/share/x", ErrPathTraversal},
		{`\\server\share\x`, ErrPathTraversal},
		{`\\?\C:\Windows\win.ini`, ErrPathTraversal},
		{`..\victim.txt`, ErrPathTraversal},
		{`db\..\..\victim.txt`, ErrPathTraversal},
		{`db\file.gz`, ErrPathTraversal},
		{`C:\Windows\win.ini`, ErrPathTraversal},
		{`C:/Windows/win.ini`, ErrPathTraversal},
		{`c:relative.gz`, ErrPathTraversal},
		{"db/x.gz\x00.txt", ErrInvalidKey},
		{"\x00", ErrInvalidKey},
		{"db/line\nbreak.gz", ErrInvalidKey},
		{"db/\rcarriage.gz", ErrInvalidKey},
		{"db/esc\x1b.gz", ErrInvalidKey},
		{"db/del\x7f.gz", ErrInvalidKey},
		{"", ErrInvalidKey},
		{".", ErrInvalidKey},
	} {
		for name, op := range localOps(s) {
			t.Run(name+"/"+tc.key, func(t *testing.T) {
				if err := op(ctx, tc.key); !errors.Is(err, tc.want) {
					t.Fatalf("%s(%q) = %v; want %v", name, tc.key, err, tc.want)
				}
			})
		}
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "do not touch" {
		t.Fatalf("the file outside the root was changed: %q, %v", raw, err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("hostile keys created files in the root: %v", names)
	}
	if names := dirEntries(t, outside); len(names) != 1 {
		t.Fatalf("hostile keys created files outside the root: %v", names)
	}
}

// TestLocalStorageFollowsSymlinkedSubdirectories checks that a subdirectory of the
// root that is a symbolic link to another location (a second disk, a NAS mount) is
// used like any other directory: links are followed, only the key itself is checked.
func TestLocalStorageFollowsSymlinkedSubdirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	otherDisk := t.TempDir()
	s, dir := newTestLocalStorage(t)
	if err := os.Symlink(otherDisk, filepath.Join(dir, "shop")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Save(ctx, "shop/2026/09/x.archive", strings.NewReader("data")); err != nil {
		t.Fatalf("Save through a linked subdirectory: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(otherDisk, "2026", "09", "x.archive")); err != nil || string(raw) != "data" {
		t.Fatalf("archive on the other disk = %q, %v", raw, err)
	}
	for name, op := range localOps(s) {
		if name == "Save" || name == "Delete" {
			continue
		}
		if err := op(ctx, "shop/2026/09/x.archive"); err != nil {
			t.Errorf("%s through a linked subdirectory: %v", name, err)
		}
	}
	if err := s.Delete(ctx, "shop/2026/09/x.archive"); err != nil {
		t.Fatalf("Delete through a linked subdirectory: %v", err)
	}
	// The lexical checks still apply to keys that name the link.
	if _, err := s.Retrieve(ctx, "shop/../../x"); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("traversal past the link: %v", err)
	}
}

// TestLocalStorageFollowsLinksWithinTheRoot checks that confinement does not break
// legitimate layouts: a root that is itself a symbolic link (a mounted NAS path), and
// links that stay inside the root.
func TestLocalStorageFollowsLinksWithinTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	rootDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "backups")
	if err := os.Symlink(rootDir, link); err != nil {
		t.Fatal(err)
	}
	s, err := NewLocalStorage(link)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.Save(ctx, "db/2026/x.archive", strings.NewReader("data")); err != nil {
		t.Fatalf("Save through a linked root: %v", err)
	}
	if err = os.Symlink("db", filepath.Join(rootDir, "alias")); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Retrieve(ctx, "alias/2026/x.archive")
	if err != nil {
		t.Fatalf("Retrieve through an inner link: %v", err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(raw) != "data" {
		t.Fatalf("read %q", raw)
	}
}
