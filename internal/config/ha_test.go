package config

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHADefaultsAreOff(t *testing.T) {
	cfg, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HA.Enabled() || cfg.HA.Cluster != DefaultHACluster || cfg.HA.LeaseTTL != 30*time.Second ||
		cfg.HA.RenewInterval != 10*time.Second || cfg.HA.SnapshotInterval != 5*time.Minute {
		t.Fatalf("HA defaults = %+v", cfg.HA)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestHAFromEnv(t *testing.T) {
	env := map[string]string{
		EnvHAMode: "Active-Passive", EnvHATarget: "s3://bucket/ha?endpoint=http://minio:9000&path_style=true",
		EnvHACluster: "prod", EnvHALeaseTTL: "20s", EnvHARenewInterval: "5s", EnvHASnapshotInterval: "1m",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.HA.Enabled() || cfg.HA.Cluster != "prod" || cfg.HA.LeaseTTL != 20*time.Second || cfg.HA.RenewInterval != 5*time.Second || cfg.HA.SnapshotInterval != time.Minute {
		t.Fatalf("HA = %+v", cfg.HA)
	}
	if _, err = FromEnv(func(k string) string {
		if k == EnvHALeaseTTL {
			return "soon"
		}
		return ""
	}); !errors.Is(err, ErrInvalidHA) {
		t.Fatalf("an invalid duration = %v; want ErrInvalidHA", err)
	}
}

func TestHAValidate(t *testing.T) {
	ok := HAConfig{Mode: HAModeActivePassive, Target: "Backups S3", Cluster: "prod", LeaseTTL: 30 * time.Second,
		RenewInterval: 10 * time.Second, SnapshotInterval: 5 * time.Minute}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*HAConfig){
		"mode":            func(h *HAConfig) { h.Mode = "active-active" },
		"no target":       func(h *HAConfig) { h.Target = " " },
		"bad url":         func(h *HAConfig) { h.Target = "gs://bucket" },
		"cluster":         func(h *HAConfig) { h.Cluster = "Prod EU" },
		"renew too long":  func(h *HAConfig) { h.RenewInterval = 15 * time.Second },
		"ttl too short":   func(h *HAConfig) { h.LeaseTTL, h.RenewInterval = 2*time.Second, time.Second },
		"snapshot":        func(h *HAConfig) { h.SnapshotInterval = time.Second },
		"credentials url": func(h *HAConfig) { h.Target = "s3://key:secret@bucket" },
	} {
		h := ok
		mutate(&h)
		if err := h.Validate(); !errors.Is(err, ErrInvalidHA) {
			t.Errorf("%s: Validate = %v; want ErrInvalidHA", name, err)
		}
	}
	if err := (HAConfig{Mode: HAModeOff}).Validate(); err != nil {
		t.Fatalf("HA off: %v", err)
	}
}

func TestParseHATarget(t *testing.T) {
	tg, err := ParseHATarget("s3://bkt/some/prefix/?endpoint=https://s3.example.com&region=eu-west-1&path_style=true")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Scheme != "s3" || tg.Bucket != "bkt" || tg.Prefix != "some/prefix" || tg.Endpoint != "https://s3.example.com" || tg.Region != "eu-west-1" || !tg.PathStyle {
		t.Fatalf("ParseHATarget = %+v", tg)
	}
	if tg, err = ParseHATarget("tgt_123"); err != nil || tg.Ref != "tgt_123" {
		t.Fatalf("ParseHATarget(ref) = %+v, %v", tg, err)
	}
	if _, err = ParseHATarget("s3://bkt?endpoint=ftp://x"); err == nil {
		t.Fatal("a non-http endpoint was accepted")
	}
	// A secret with a '/' after a numeric first part must not become a bucket
	// "key:123" with the rest of the secret as the prefix (found by FuzzHATargetLabel).
	if _, err = ParseHATarget("s3://key:123/secret@bucket"); !errors.Is(err, ErrInvalidHA) {
		t.Fatalf("a bucket with a port was accepted: %v", err)
	}
	if label := HATargetLabel("s3://key:123/secret@bucket"); strings.Contains(label, "secret") {
		t.Fatalf("HATargetLabel leaks the secret: %q", label)
	}
	if _, err = ParseHATarget("file://relative/dir"); runtime.GOOS != "windows" && err == nil {
		t.Fatal("a file location with a host was accepted outside Windows (where it is a UNC path)")
	}
}
