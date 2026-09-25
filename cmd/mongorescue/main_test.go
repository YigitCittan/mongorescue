package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestFlagsOverrideBootstrapEnvironment(t *testing.T) {
	var stderr bytes.Buffer
	cfg, level, version, err := parseFlags([]string{"-port", "9090", "-log-level", "debug"},
		env(map[string]string{config.EnvDataDir: "/srv/mr", config.EnvHost: "127.0.0.1", config.EnvPort: "8081"}), &stderr)
	if err != nil || version {
		t.Fatalf("parseFlags = %v, %v", err, version)
	}
	if cfg.DataDir != filepath.FromSlash("/srv/mr") || cfg.Host != "127.0.0.1" || cfg.Port != 9090 || level != slog.LevelDebug {
		t.Fatalf("cfg = %+v, level %v", cfg, level)
	}
	cfg, _, _, err = parseFlags([]string{"-data-dir", "/tmp/x", "-host", "::1"}, env(nil), &stderr)
	if err != nil || cfg.DataDir != filepath.FromSlash("/tmp/x") || cfg.Host != "::1" || cfg.Port != config.DefaultPort {
		t.Fatalf("flags only = %+v, %v", cfg, err)
	}
}

func TestFlagErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"-config", "x.json"}, nil, "flag provided but not defined"},
		{[]string{"-gen-age-key"}, nil, "flag provided but not defined"},
		{[]string{"-port", "0"}, nil, "port"},
		{[]string{"-log-level", "loud"}, nil, "log level"},
		{[]string{"extra"}, nil, "unexpected arguments"},
		{nil, map[string]string{config.EnvPort: "http"}, "port"},
		{nil, map[string]string{config.EnvSecretKey: "not-a-key"}, config.EnvSecretKey},
	} {
		var stderr bytes.Buffer
		_, _, _, err := parseFlags(tc.args, env(tc.env), &stderr)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseFlags(%v, %v) = %v; want %q", tc.args, tc.env, err, tc.want)
		}
		if strings.Contains(err.Error(), "not-a-key") {
			t.Errorf("error echoes the secret key: %v", err)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// -version works even when the environment is invalid.
	if code := run([]string{"-version"}, env(map[string]string{config.EnvPort: "bad"}), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "MongoRescue v") {
		t.Fatalf("version output = %q", stdout.String())
	}
	if code := run([]string{"-bogus"}, env(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown flag exit code = %d; want 2", code)
	}
}
