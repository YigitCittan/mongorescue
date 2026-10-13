package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

func TestKeyWrapFromEnv(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{EnvKeyWrap: " vault-transit:https://vault:8200/transit/keys/mongorescue "}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyWrap != (keywrap.Spec{Provider: keywrap.ProviderVaultTransit, KeyURI: "https://vault:8200/transit/keys/mongorescue"}) {
		t.Fatalf("KeyWrap = %+v", cfg.KeyWrap)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	_, err = FromEnv(env(map[string]string{EnvKeyWrap: "vault-transit:https://root:hunter2@vault:8200/transit/keys/k"}))
	if !errors.Is(err, keywrap.ErrInvalidSpec) || !strings.Contains(err.Error(), EnvKeyWrap) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("invalid MONGORESCUE_KEY_WRAP = %v", err)
	}

	key, _ := secretbox.GenerateKey()
	cfg, err = FromEnv(env(map[string]string{EnvKeyWrap: "aws-kms:alias/mongorescue", EnvSecretKey: secretbox.EncodeKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); !errors.Is(err, ErrKeyWrapWithSecretKey) {
		t.Fatalf("Validate with both = %v; want ErrKeyWrapWithSecretKey", err)
	}

	cfg = Default()
	cfg.KeyWrap = keywrap.Spec{Provider: keywrap.ProviderGCPKMS, KeyURI: "not a key"}
	if err = cfg.Validate(); !errors.Is(err, keywrap.ErrInvalidSpec) {
		t.Fatalf("Validate with an invalid spec = %v", err)
	}
}

func TestKeyWrapPlainHTTPNeedsTheOptIn(t *testing.T) {
	vars := map[string]string{EnvKeyWrap: "vault-transit:http://127.0.0.1:8200/transit/keys/k"}
	cfg, err := FromEnv(env(vars))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); !errors.Is(err, keywrap.ErrInsecureKeyURI) {
		t.Fatalf("http:// without the opt-in = %v; want ErrInsecureKeyURI", err)
	}
	vars[EnvKeyWrapAllowInsecure] = "true"
	if cfg, err = FromEnv(env(vars)); err != nil || !cfg.KeyWrapAllowInsecure {
		t.Fatalf("FromEnv with the opt-in = %+v, %v", cfg, err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatalf("http:// with the opt-in = %v", err)
	}
	vars[EnvKeyWrapAllowInsecure] = "maybe"
	if _, err = FromEnv(env(vars)); err == nil {
		t.Fatal("an invalid opt-in value was accepted")
	}
}
