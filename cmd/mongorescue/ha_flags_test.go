package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/config"
)

func TestHAFlags(t *testing.T) {
	var stderr bytes.Buffer
	target := config.FileURL(t.TempDir())
	cfg, _, _, err := parseFlags([]string{"-ha-mode", "active-passive", "-ha-target", target, "-ha-cluster", "prod",
		"-ha-lease-ttl", "20s", "-ha-renew-interval", "5s", "-ha-snapshot-interval", "2m"}, env(nil), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HA.Enabled() || cfg.HA.Target != target || cfg.HA.Cluster != "prod" ||
		cfg.HA.LeaseTTL != 20*time.Second || cfg.HA.RenewInterval != 5*time.Second || cfg.HA.SnapshotInterval != 2*time.Minute {
		t.Fatalf("HA = %+v", cfg.HA)
	}
	if _, _, _, err = parseFlags([]string{"-ha-lease-ttl", "later"}, env(nil), &stderr); err == nil || !strings.Contains(err.Error(), "-ha-lease-ttl") {
		t.Fatalf("an invalid -ha-lease-ttl = %v", err)
	}
	if _, _, _, err = parseFlags([]string{"-ha-mode", "active-passive"}, env(nil), &stderr); err == nil {
		t.Fatal("active-passive without a target was accepted")
	}
}
