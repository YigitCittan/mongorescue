package desktop

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseConfig(t *testing.T) {
	var stderr bytes.Buffer
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // os.UserConfigDir on Linux
	def, err := DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(def, filepath.Join(AppDirName, "data")) {
		t.Fatalf("DefaultDataDir = %s", def)
	}

	cfg, err := ParseConfig(nil, envMap(nil), &stderr)
	if err != nil || cfg.DataDir != def || !cfg.Dashboard || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	cfg, err = ParseConfig(nil, envMap(map[string]string{config.EnvDataDir: "/srv/mr/"}), &stderr)
	if err != nil || cfg.DataDir != filepath.FromSlash("/srv/mr") {
		t.Fatalf("env data dir = %+v, %v", cfg, err)
	}
	cfg, err = ParseConfig([]string{"-data-dir", "/tmp/x", "-log-level", "debug"},
		envMap(map[string]string{config.EnvDataDir: "/srv/mr", config.EnvDashboard: "false"}), &stderr)
	if err != nil || cfg.DataDir != filepath.FromSlash("/tmp/x") || cfg.LogLevel != slog.LevelDebug || !cfg.Dashboard {
		t.Fatalf("flags = %+v, %v", cfg, err)
	}
	// Server-only variables are ignored, even when invalid.
	cfg, err = ParseConfig(nil, envMap(map[string]string{config.EnvPort: "http", config.EnvDashboard: "maybe", config.EnvHost: "a b"}), &stderr)
	if err != nil || !cfg.Dashboard || cfg.DataDir != def {
		t.Fatalf("server-only env = %+v, %v", cfg, err)
	}
	tools := t.TempDir()
	cfg, err = ParseConfig(nil, envMap(map[string]string{config.EnvToolsDir: tools}), &stderr)
	if err != nil || cfg.ToolsDir != tools {
		t.Fatalf("env tools dir = %+v, %v", cfg, err)
	}
	if _, err := ParseConfig(nil, envMap(map[string]string{config.EnvToolsDir: "tools"}), &stderr); !errors.Is(err, config.ErrInvalidToolsDir) {
		t.Fatalf("relative MONGORESCUE_TOOLS_DIR: err = %v, want ErrInvalidToolsDir", err)
	}
	if _, err := ParseConfig(nil, envMap(map[string]string{config.EnvSecretKey: "short"}), &stderr); err == nil {
		t.Fatal("invalid MONGORESCUE_SECRET_KEY accepted")
	}
	for _, args := range [][]string{{"extra"}, {"-log-level", "loud"}, {"-port", "1"}} {
		if _, err := ParseConfig(args, envMap(nil), &stderr); err == nil {
			t.Errorf("ParseConfig(%v) = nil error", args)
		}
	}
}

func TestNewLogger(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	var stderr bytes.Buffer
	logger, closer, err := NewLogger(cfg, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello desktop")
	if err = closer.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "desktop.log"))
	if err != nil || !strings.Contains(string(b), "hello desktop") || !strings.Contains(stderr.String(), "hello desktop") {
		t.Fatalf("log file %q, stderr %q, %v", b, stderr.String(), err)
	}
}

func TestIsVersionFlag(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{nil, false},
		{[]string{"--version", "--hidden"}, false},
		{[]string{"-data-dir", "--version"}, false},
		{[]string{"--versions"}, false},
	}
	for _, tt := range tests {
		if got := IsVersionFlag(tt.args); got != tt.want {
			t.Errorf("IsVersionFlag(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}
