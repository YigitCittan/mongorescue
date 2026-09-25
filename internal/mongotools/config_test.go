package mongotools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteURIConfig(t *testing.T) {
	const uri = "mongodb://u:it's-secret@h:27017/db?authSource=admin"

	arg, cleanup, err := WriteURIConfig(t.TempDir(), uri)
	if err != nil {
		t.Fatalf("WriteURIConfig: %v", err)
	}
	defer cleanup()

	path, ok := strings.CutPrefix(arg, "--config=")
	if !ok {
		t.Fatalf("expected --config= argument, got %q", arg)
	}
	if strings.Contains(arg, "secret") {
		t.Fatal("argument must not contain the URI")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %v", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "uri: 'mongodb://u:it''s-secret@h:27017/db?authSource=admin'\n"; string(data) != want {
		t.Fatalf("config content = %q; want %q", data, want)
	}

	cleanup()
	cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file to be removed, stat err = %v", err)
	}
}

func TestWriteURIConfigRejectsControlChars(t *testing.T) {
	if _, _, err := WriteURIConfig(t.TempDir(), "mongodb://h/\nuri: evil"); !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("expected ErrInvalidURI, got %v", err)
	}
}

func TestCleanupStale(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)

	write := func(name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("uri: 'x'\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		return p
	}

	staleA := write("mongorescue-tools-111.yaml", old)
	staleB := write("mongorescue-tools-222.yaml", old)
	fresh := write("mongorescue-tools-333.yaml", time.Now())
	unrelated := write("other-tools-444.yaml", old)
	wrongExt := write("mongorescue-tools-555.yml", old)

	staleDir := filepath.Join(dir, "mongorescue-tools-dir.yaml")
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}

	// A file written through WriteURIConfig must match the cleanup pattern.
	arg, cleanup, err := WriteURIConfig(dir, "mongodb://h")
	if err != nil {
		t.Fatalf("WriteURIConfig: %v", err)
	}
	defer cleanup()
	generated := strings.TrimPrefix(arg, "--config=")
	if err = os.Chtimes(generated, old, old); err != nil {
		t.Fatalf("chtimes generated: %v", err)
	}

	removed, err := CleanupStale(dir, time.Minute)
	if err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if removed != 3 {
		t.Fatalf("expected 3 files removed, got %d", removed)
	}

	tests := []struct {
		path string
		gone bool
	}{
		{path: staleA, gone: true},
		{path: staleB, gone: true},
		{path: generated, gone: true},
		{path: fresh, gone: false},
		{path: unrelated, gone: false},
		{path: wrongExt, gone: false},
		{path: staleDir, gone: false},
	}
	for _, tt := range tests {
		_, statErr := os.Stat(tt.path)
		if gone := errors.Is(statErr, os.ErrNotExist); gone != tt.gone {
			t.Errorf("%s: gone=%v, want %v", filepath.Base(tt.path), gone, tt.gone)
		}
	}

	// Idempotent: nothing left to remove.
	if removed, err := CleanupStale(dir, time.Minute); err != nil || removed != 0 {
		t.Fatalf("second CleanupStale: removed=%d err=%v", removed, err)
	}
}
