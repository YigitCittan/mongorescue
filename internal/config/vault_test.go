package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/vault"
)

func TestVaultFromEnv(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil || cfg.Vault.Enabled() {
		t.Fatalf("default = %+v, %v", cfg.Vault, err)
	}
	cfg, err = FromEnv(env(map[string]string{EnvVaultAddr: " https://vault.example.com:8200 ", EnvVaultCacheTTL: "90s"}))
	if err != nil || cfg.Vault.Addr != "https://vault.example.com:8200" || cfg.Vault.CacheTTL != 90*time.Second {
		t.Fatalf("vault = %+v, %v", cfg.Vault, err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0s", "25h", "soon"} {
		if _, err = FromEnv(env(map[string]string{EnvVaultCacheTTL: v})); !errors.Is(err, ErrInvalidVaultCacheTTL) {
			t.Errorf("cache TTL %q = %v", v, err)
		}
	}
	if _, err = FromEnv(env(map[string]string{EnvVaultAllowInsecure: "maybe"})); err == nil {
		t.Error("an invalid allow-insecure value was accepted")
	}
}

func TestVaultPlainHTTPNeedsTheOptIn(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{EnvVaultAddr: "http://127.0.0.1:8200"}))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); !errors.Is(err, vault.ErrInsecureAddr) || !strings.Contains(err.Error(), EnvVaultAllowInsecure) {
		t.Fatalf("http without the opt-in = %v", err)
	}
	cfg.Vault.AllowInsecure = true
	if err = cfg.Validate(); err != nil {
		t.Fatalf("http with the opt-in = %v", err)
	}
	cfg.Vault.Addr = "https://vault:8200/v1/secret"
	if err = cfg.Validate(); !errors.Is(err, vault.ErrInvalidAddr) {
		t.Fatalf("address with a path = %v", err)
	}
}
