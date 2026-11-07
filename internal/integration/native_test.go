//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

// TestNativeToolchain checks what depends on the operating system and cannot be
// seen from a Linux container: the tools are found by their real file names (with
// ".exe" on Windows), they are at least mongotools.MinToolsVersion, and the real
// mongodump reads the credentials from the private configuration file that
// WriteURIConfig writes (which never reaches the argument list) and the file is
// removed afterwards. scripts/test-native-e2e.sh runs it on Windows and macOS
// against a mongod with access control; it passes on Linux too.
func TestNativeToolchain(t *testing.T) {
	env := requireMongo(t)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	resolver := mongotools.NewResolver("")

	var dump string
	for _, name := range []string{"mongodump", "mongorestore"} {
		path, err := resolver.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if !filepath.IsAbs(path) {
			t.Errorf("%s resolved to the relative path %q", name, path)
		}
		if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(path), ".exe") {
			t.Errorf("%s resolved to %q; want an .exe on Windows", name, path)
		}
		version, err := resolver.ToolVersion(ctx, name)
		if err != nil {
			t.Fatalf("%s version: %v", name, err)
		}
		if !mongotools.VersionAtLeast(version, mongotools.MinToolsVersion) {
			t.Errorf("%s %s is older than the supported minimum %s", name, version, mongotools.MinToolsVersion)
		}
		t.Logf("%s %s at %s", name, version, path)
		if name == "mongodump" {
			dump = path
		}
	}

	dir := t.TempDir()
	arg, cleanup, err := mongotools.WriteURIConfig(dir, env.URI)
	if err != nil {
		t.Fatalf("WriteURIConfig: %v", err)
	}
	defer cleanup()
	cfg, ok := strings.CutPrefix(arg, "--config=")
	if !ok {
		t.Fatalf("argument %q is not --config=<path>", arg)
	}
	if env.Password != "" && strings.Contains(arg, env.Password) {
		t.Fatalf("the credentials are in the argument %q", arg)
	}
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat the config file: %v", err)
	}
	// Windows does not report Unix permission bits; the file sits in the per-user
	// temporary directory there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode %v; want 0600", info.Mode().Perm())
	}

	// The real tool authenticates with it: dump admin.system.version to a file.
	archive := filepath.Join(dir, "version.archive")
	cmd := mongotools.Command(ctx, dump, arg, "--db", "admin", "--collection", "system.version", "--archive="+archive)
	if err := mongotools.Start(cmd); err != nil {
		t.Fatalf("start mongodump: %v", err)
	}
	if err := mongotools.Wait(ctx, cmd); err != nil {
		t.Fatalf("mongodump with the config file: %v", err)
	}
	if st, err := os.Stat(archive); err != nil || st.Size() == 0 {
		t.Fatalf("mongodump wrote no archive: %v", err)
	}

	cleanup()
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("the config file still exists after cleanup: %v", err)
	}
}
