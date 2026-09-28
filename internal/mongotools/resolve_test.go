package mongotools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTool creates a file with mode at path (and its parent directories).
func writeTool(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolverSearchOrder(t *testing.T) {
	notOnPath := func(string) (string, error) { return "", errors.New("not on PATH") }

	cases := []struct {
		name string
		goos string
		// setup creates files under root; dir and exe are relative to root.
		setup    func(t *testing.T, root string)
		dir      string
		exe      string
		lookPath func(string) (string, error)
		want     string // relative to root; "" means ErrToolNotFound; "PATH" means lookPath's result
	}{
		{
			name: "configured dir wins over bundled",
			goos: "linux",
			setup: func(t *testing.T, root string) {
				writeTool(t, filepath.Join(root, "custom", "mongodump"), 0o755)
				writeTool(t, filepath.Join(root, "app", "tools", "mongodump"), 0o755)
			},
			dir:      "custom",
			exe:      "app/mongorescue",
			lookPath: notOnPath,
			want:     "custom/mongodump",
		},
		{
			name: "configured dir without the tool falls back to bundled",
			goos: "linux",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "custom"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeTool(t, filepath.Join(root, "app", "tools", "mongodump"), 0o755)
			},
			dir:      "custom",
			exe:      "app/mongorescue",
			lookPath: notOnPath,
			want:     "app/tools/mongodump",
		},
		{
			name: "windows appends .exe",
			goos: "windows",
			setup: func(t *testing.T, root string) {
				writeTool(t, filepath.Join(root, "app", "tools", "mongodump"), 0o755)
				writeTool(t, filepath.Join(root, "app", "tools", "mongodump.exe"), 0o755)
			},
			exe:      "app/MongoRescue.exe",
			lookPath: notOnPath,
			want:     "app/tools/mongodump.exe",
		},
		{
			name: "darwin app bundle resources",
			goos: "darwin",
			setup: func(t *testing.T, root string) {
				writeTool(t, filepath.Join(root, "MongoRescue.app", "Contents", "Resources", "tools", "mongodump"), 0o755)
			},
			exe:      "MongoRescue.app/Contents/MacOS/MongoRescue",
			lookPath: notOnPath,
			want:     "MongoRescue.app/Contents/Resources/tools/mongodump",
		},
		{
			name: "resources dir ignored outside darwin",
			goos: "linux",
			setup: func(t *testing.T, root string) {
				writeTool(t, filepath.Join(root, "MongoRescue.app", "Contents", "Resources", "tools", "mongodump"), 0o755)
			},
			exe:      "MongoRescue.app/Contents/MacOS/MongoRescue",
			lookPath: notOnPath,
			want:     "",
		},
		{
			name: "darwin resources dir needs a Contents/MacOS executable",
			goos: "darwin",
			setup: func(t *testing.T, root string) {
				writeTool(t, filepath.Join(root, "Resources", "tools", "mongodump"), 0o755)
			},
			exe:      "bin/mongorescue",
			lookPath: notOnPath,
			want:     "",
		},
		{
			name: "directory with the tool name does not count",
			goos: "linux",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "app", "tools", "mongodump"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			exe:      "app/mongorescue",
			lookPath: notOnPath,
			want:     "",
		},
		{
			name:  "falls back to PATH",
			goos:  "linux",
			setup: func(*testing.T, string) {},
			exe:   "app/mongorescue",
			lookPath: func(name string) (string, error) {
				return "/opt/bin/" + name, nil
			},
			want: "PATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			r := &Resolver{
				GOOS:       tc.goos,
				Executable: func() (string, error) { return filepath.Join(root, filepath.FromSlash(tc.exe)), nil },
				LookPath:   tc.lookPath,
			}
			if tc.dir != "" {
				r.Dir = filepath.Join(root, tc.dir)
			}
			got, err := r.Resolve("mongodump")
			switch tc.want {
			case "":
				if !errors.Is(err, ErrToolNotFound) {
					t.Fatalf("Resolve = %q, %v; want ErrToolNotFound", got, err)
				}
			case "PATH":
				if err != nil || got != mustAbs(t, "/opt/bin/mongodump") {
					t.Fatalf("Resolve = %q, %v; want PATH result", got, err)
				}
			default:
				want := mustEval(t, filepath.Join(root, filepath.FromSlash(tc.want)))
				if err != nil || mustEval(t, got) != want {
					t.Fatalf("Resolve = %q, %v; want %q", got, err, want)
				}
			}
		})
	}
}

func TestResolverSkipsNonExecutable(t *testing.T) {
	if isWindowsHost() {
		t.Skip("no execute bit on Windows")
	}
	root := t.TempDir()
	writeTool(t, filepath.Join(root, "tools", "mongorestore"), 0o644)
	r := &Resolver{
		GOOS:       "linux",
		Executable: func() (string, error) { return filepath.Join(root, "mongorescue"), nil },
		LookPath:   func(string) (string, error) { return "", errors.New("no") },
	}
	if _, err := r.Resolve("mongorestore"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Resolve err = %v, want ErrToolNotFound", err)
	}
}

func TestToolNotFoundErrorMessage(t *testing.T) {
	root := t.TempDir()
	r := &Resolver{
		Dir:        filepath.Join(root, "custom"),
		GOOS:       "darwin",
		Executable: func() (string, error) { return filepath.Join(root, "App.app", "Contents", "MacOS", "App"), nil },
		LookPath:   func(string) (string, error) { return "", errors.New("no") },
	}
	_, err := r.Resolve("mongodump")
	var nf *ToolNotFoundError
	if !errors.As(err, &nf) || !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("err = %v, want *ToolNotFoundError wrapping ErrToolNotFound", err)
	}
	if want := "mongodump not found: install MongoDB Database Tools or set " + EnvToolsDir; err.Error() != want {
		t.Errorf("message = %q, want %q (no local paths)", err.Error(), want)
	}
	// The executable does not exist, so its path is used without resolving symlinks.
	want := []string{
		filepath.Join(root, "custom", "mongodump"),
		filepath.Join(root, "App.app", "Contents", "MacOS", "tools", "mongodump"),
		filepath.Join(root, "App.app", "Contents", "Resources", "tools", "mongodump"),
		"PATH",
	}
	if strings.Join(nf.Searched, "|") != strings.Join(want, "|") {
		t.Errorf("Searched = %v, want %v", nf.Searched, want)
	}
}

func TestResolverIgnoresRelativeDir(t *testing.T) {
	root := t.TempDir()
	writeTool(t, filepath.Join(root, "tools", "mongodump"), 0o755)
	t.Chdir(root)
	for _, dir := range []string{".", "tools", "./tools"} {
		r := &Resolver{
			Dir:        dir,
			GOOS:       "linux",
			Executable: func() (string, error) { return "", errors.New("no executable") },
			LookPath:   func(string) (string, error) { return "", errors.New("no") },
		}
		if got, err := r.Resolve("mongodump"); !errors.Is(err, ErrToolNotFound) {
			t.Errorf("Dir %q: Resolve = %q, %v; want ErrToolNotFound", dir, got, err)
		}
	}
}

func TestResolveExplicitPath(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "mongodump")
	writeTool(t, tool, 0o755)
	r := &Resolver{LookPath: func(string) (string, error) { t.Fatal("PATH searched for explicit path"); return "", nil }}
	if got, err := r.Resolve(tool); err != nil || got != tool {
		t.Fatalf("Resolve(%q) = %q, %v", tool, got, err)
	}
	if _, err := r.Resolve(filepath.Join(root, "missing")); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("missing explicit path err = %v, want ErrToolNotFound", err)
	}
}

func isWindowsHost() bool { return os.PathSeparator == '\\' }

func mustEval(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return resolved
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
