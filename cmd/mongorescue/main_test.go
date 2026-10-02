package main

import (
	"bytes"
	"errors"
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
	if cfg.Dashboard {
		t.Fatal("dashboard enabled by default")
	}
	cfg, _, _, err = parseFlags(nil, env(map[string]string{config.EnvDashboard: "true"}), &stderr)
	if err != nil || !cfg.Dashboard {
		t.Fatalf("env dashboard = %+v, %v", cfg, err)
	}
	cfg, _, _, err = parseFlags([]string{"-dashboard=false"}, env(map[string]string{config.EnvDashboard: "true"}), &stderr)
	if err != nil || cfg.Dashboard {
		t.Fatalf("-dashboard=false over env = %+v, %v", cfg, err)
	}
	cfg, _, _, err = parseFlags([]string{"-dashboard"}, env(nil), &stderr)
	if err != nil || !cfg.Dashboard {
		t.Fatalf("-dashboard = %+v, %v", cfg, err)
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
		{nil, map[string]string{config.EnvDashboard: "maybe"}, config.EnvDashboard},
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

func TestToolsDirFlag(t *testing.T) {
	var stderr bytes.Buffer
	abs := t.TempDir()
	envAbs := t.TempDir()
	for _, tc := range []struct {
		name    string
		args    []string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "unset", want: ""},
		{name: "empty flag clears env", args: []string{"-tools-dir", ""}, env: map[string]string{config.EnvToolsDir: envAbs}, want: ""},
		{name: "absolute flag over env", args: []string{"-tools-dir", abs}, env: map[string]string{config.EnvToolsDir: envAbs}, want: abs},
		{name: "absolute env", env: map[string]string{config.EnvToolsDir: envAbs}, want: envAbs},
		{name: "dot flag", args: []string{"-tools-dir", "."}, wantErr: true},
		{name: "relative flag", args: []string{"-tools-dir", "tools"}, wantErr: true},
		{name: "relative env", env: map[string]string{config.EnvToolsDir: "tools"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, err := parseFlags(tc.args, env(tc.env), &stderr)
			if tc.wantErr {
				if !errors.Is(err, config.ErrInvalidToolsDir) {
					t.Fatalf("err = %v, want ErrInvalidToolsDir", err)
				}
				return
			}
			if err != nil || cfg.ToolsDir != tc.want {
				t.Fatalf("ToolsDir = %q, %v; want %q", cfg.ToolsDir, err, tc.want)
			}
		})
	}
}

// TestDispatch proves the CLI commands do not change the server flags or the mcp
// subcommand: only the CLI command names are taken from the server.
func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		env        map[string]string
		code       int
		wantOut    string
		wantErr    string
		notWantErr string
	}{
		{name: "server version", args: []string{"--version"}, code: 0, wantOut: "MongoRescue v"},
		{name: "server positional", args: []string{"extra"}, code: 2, wantErr: "unexpected arguments"},
		{name: "server bad flag", args: []string{"-port", "0"}, code: 2, wantErr: "port"},
		{name: "mcp help", args: []string{"mcp", "-h"}, code: 0, wantErr: "Usage: mongorescue mcp"},
		{name: "mcp positional", args: []string{"mcp", "extra"}, code: 2, wantErr: "unexpected arguments"},
		{name: "cli help", args: []string{"help"}, code: 0, wantOut: "Usage: mongorescue <command>"},
		{name: "cli without key", args: []string{"status"}, code: 2, wantErr: "no API key"},
		{
			name: "cli never reads the server's key", args: []string{"list", "backups"},
			env:  map[string]string{"MONGORESCUE_API_KEY": "mr_admin_secret", "MONGORESCUE_API_KEY_FILE": "/nonexistent"},
			code: 2, wantErr: "no API key", notWantErr: "mr_admin_secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := dispatch(tc.args, env(tc.env), strings.NewReader(""), &stdout, &stderr)
			if code != tc.code || !strings.Contains(stdout.String(), tc.wantOut) || !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("dispatch(%v) = %d\nstdout: %s\nstderr: %s", tc.args, code, stdout.String(), stderr.String())
			}
			if tc.notWantErr != "" && strings.Contains(stderr.String()+stdout.String(), tc.notWantErr) {
				t.Fatalf("output contains %q", tc.notWantErr)
			}
		})
	}
}
