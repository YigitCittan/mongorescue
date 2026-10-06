package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
)

func TestToolsTempDir(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tools-tmp")
		got, err := toolsTempDir(&config.Config{DataDir: t.TempDir(), TmpDir: dir})
		if err != nil || got != dir {
			t.Fatalf("toolsTempDir = %q, %v; want %q", got, err, dir)
		}
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			t.Fatalf("configured directory not created: %v", statErr)
		}
	})
	t.Run("system temporary directory by default", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		t.Setenv("TMP", tmp)
		t.Setenv("TEMP", tmp)
		data := t.TempDir()
		got, err := toolsTempDir(&config.Config{DataDir: data})
		if err != nil || got != os.TempDir() {
			t.Fatalf("toolsTempDir = %q, %v; want %q", got, err, os.TempDir())
		}
		if _, statErr := os.Stat(filepath.Join(data, ToolsTempDirName)); !os.IsNotExist(statErr) {
			t.Fatal("the data directory fallback was created although the system one is writable")
		}
	})
	t.Run("data directory when the system one is not writable", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		t.Setenv("TMPDIR", missing)
		t.Setenv("TMP", missing)
		t.Setenv("TEMP", missing)
		data := t.TempDir()
		got, err := toolsTempDir(&config.Config{DataDir: data})
		want := filepath.Join(data, ToolsTempDirName)
		if err != nil || got != want {
			t.Fatalf("toolsTempDir = %q, %v; want %q", got, err, want)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != toolsTempDirPerm {
			t.Fatalf("fallback mode = %v", info.Mode().Perm())
		}
	})
}
