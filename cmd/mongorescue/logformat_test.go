package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
)

func TestLogFormatAndAccessLogFlags(t *testing.T) {
	var stderr bytes.Buffer
	cfg, _, _, err := parseFlags(nil, env(nil), &stderr)
	if err != nil || cfg.LogFormat != "text" || cfg.AccessLog {
		t.Fatalf("defaults = %q, %v, %v", cfg.LogFormat, cfg.AccessLog, err)
	}
	cfg, _, _, err = parseFlags(nil, env(map[string]string{config.EnvLogFormat: "json", config.EnvAccessLog: "true"}), &stderr)
	if err != nil || cfg.LogFormat != "json" || !cfg.AccessLog {
		t.Fatalf("env = %q, %v, %v", cfg.LogFormat, cfg.AccessLog, err)
	}
	cfg, _, _, err = parseFlags([]string{"-log-format", "text", "-access-log=false"},
		env(map[string]string{config.EnvLogFormat: "json", config.EnvAccessLog: "true"}), &stderr)
	if err != nil || cfg.LogFormat != "text" || cfg.AccessLog {
		t.Fatalf("flags over env = %q, %v, %v", cfg.LogFormat, cfg.AccessLog, err)
	}
}

func TestMCPBridgeJSONLogsRedacted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	const key = "mr_abcdefgh_secretsecretsecretsecretsecretse"
	code := runMCP([]string{"--api-key", key, "--log-format", "json", "--url", "http://127.0.0.1:1"}, env(nil), strings.NewReader(""), &stdout, &stderr)
	if code != 1 || strings.Contains(stderr.String(), "secretsecret") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Errorf("stderr line is not a JSON object: %q", line)
		}
	}
}
