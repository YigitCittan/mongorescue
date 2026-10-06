package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
)

// TestShutdownGraceFlagAndEnvironment proves the shutdown grace period is read from
// MONGORESCUE_SHUTDOWN_GRACE and -shutdown-grace (the flag wins), as a duration or
// seconds, and that invalid values are refused.
func TestShutdownGraceFlagAndEnvironment(t *testing.T) {
	var stderr bytes.Buffer
	cfg, _, _, err := parseFlags(nil, env(nil), &stderr)
	if err != nil || cfg.ShutdownGrace != 0 {
		t.Fatalf("default = %v, %v; want 0", cfg.ShutdownGrace, err)
	}
	cfg, _, _, err = parseFlags(nil, env(map[string]string{config.EnvShutdownGrace: "540"}), &stderr)
	if err != nil || cfg.ShutdownGrace != 9*time.Minute {
		t.Fatalf("env seconds = %v, %v; want 9m", cfg.ShutdownGrace, err)
	}
	cfg, _, _, err = parseFlags([]string{"-shutdown-grace", "2m30s"}, env(map[string]string{config.EnvShutdownGrace: "9m"}), &stderr)
	if err != nil || cfg.ShutdownGrace != 150*time.Second {
		t.Fatalf("flag over env = %v, %v; want 2m30s", cfg.ShutdownGrace, err)
	}
	for _, bad := range []string{"soon", "-1s", "25h", "1.5"} {
		if _, _, _, err = parseFlags([]string{"-shutdown-grace", bad}, env(nil), &stderr); err == nil || !strings.Contains(err.Error(), "shutdown grace") {
			t.Errorf("-shutdown-grace %q = %v; want an error", bad, err)
		}
		if _, _, _, err = parseFlags(nil, env(map[string]string{config.EnvShutdownGrace: bad}), &stderr); err == nil || !strings.Contains(err.Error(), config.EnvShutdownGrace) {
			t.Errorf("%s=%q = %v; want an error naming the variable", config.EnvShutdownGrace, bad, err)
		}
	}
}
