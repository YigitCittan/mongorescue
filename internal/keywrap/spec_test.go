package keywrap_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/keywrap"
)

func TestParseSpec(t *testing.T) {
	valid := map[string]keywrap.Spec{
		"":    {},
		"   ": {},
		"aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab": {Provider: "aws-kms",
			KeyURI: "arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"},
		"aws-kms:arn:aws-us-gov:kms:us-gov-west-1:123456789012:alias/mongorescue": {Provider: "aws-kms",
			KeyURI: "arn:aws-us-gov:kms:us-gov-west-1:123456789012:alias/mongorescue"},
		"AWS-KMS: alias/mongo-rescue":                  {Provider: "aws-kms", KeyURI: "alias/mongo-rescue"},
		"aws-kms:mrk-1234abcd12ab34cd56ef1234567890ab": {Provider: "aws-kms", KeyURI: "mrk-1234abcd12ab34cd56ef1234567890ab"},
		"gcp-kms:projects/p-1/locations/europe-west1/keyRings/r/cryptoKeys/k": {Provider: "gcp-kms",
			KeyURI: "projects/p-1/locations/europe-west1/keyRings/r/cryptoKeys/k"},
		"azure-keyvault:https://my-vault.vault.azure.net/keys/mongorescue": {Provider: "azure-keyvault",
			KeyURI: "https://my-vault.vault.azure.net/keys/mongorescue"},
		"azure-keyvault:https://my-vault.vault.azure.net/keys/mongorescue/0123abcd": {Provider: "azure-keyvault",
			KeyURI: "https://my-vault.vault.azure.net/keys/mongorescue/0123abcd"},
		"vault-transit:https://vault:8200/transit/keys/mongorescue": {Provider: "vault-transit",
			KeyURI: "https://vault:8200/transit/keys/mongorescue"},
		"vault-transit:http://127.0.0.1:8200/team/a/transit/keys/k": {Provider: "vault-transit",
			KeyURI: "http://127.0.0.1:8200/team/a/transit/keys/k"},
	}
	for in, want := range valid {
		got, err := keywrap.ParseSpec(in)
		if err != nil || got != want {
			t.Errorf("ParseSpec(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	invalid := []string{
		"aws-kms",
		"aws-kms:",
		"kms:arn:aws:kms:eu-west-1:123456789012:key/abc",
		"aws-kms:arn:aws:s3:::bucket",
		"aws-kms:not a key",
		"gcp-kms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"gcp-kms:projects/p/keyRings/r",
		"azure-keyvault:http://my-vault.vault.azure.net/keys/k",
		"azure-keyvault:https://user:s3cr3t@my-vault.vault.azure.net/keys/k",
		"azure-keyvault:https://my-vault.vault.azure.net/secrets/k",
		"azure-keyvault:https://my-vault.vault.azure.net/keys/k?api-version=1",
		"vault-transit:https://vault:8200/keys/k",
		"vault-transit:https://vault:8200/v1/transit/keys/k",
		"vault-transit:https://root:hunter2@vault:8200/transit/keys/k",
		"vault-transit:https://vault:8200/transit/keys/k?token=hunter2",
		"vault-transit:ftp://vault/transit/keys/k",
	}
	for _, in := range invalid {
		_, err := keywrap.ParseSpec(in)
		if !errors.Is(err, keywrap.ErrInvalidSpec) {
			t.Errorf("ParseSpec(%q) = %v; want ErrInvalidSpec", in, err)
			continue
		}
		for _, secret := range []string{"s3cr3t", "hunter2"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("ParseSpec(%q) error leaks the credential: %v", in, err)
			}
		}
	}
}

func TestSpecStringAndSplit(t *testing.T) {
	spec, err := keywrap.ParseSpec("vault-transit:https://vault.internal:8200/ns/transit/keys/mongorescue")
	if err != nil {
		t.Fatal(err)
	}
	if spec.String() != "vault-transit:https://vault.internal:8200/ns/transit/keys/mongorescue" {
		t.Errorf("String = %q", spec.String())
	}
	addr, mount, name, err := keywrap.SplitVaultKeyURI(spec.KeyURI)
	if err != nil || addr != "https://vault.internal:8200" || mount != "ns/transit" || name != "mongorescue" {
		t.Errorf("SplitVaultKeyURI = %q %q %q %v", addr, mount, name, err)
	}
	vaultURL, key, version, err := keywrap.SplitAzureKeyURI("https://v.vault.azure.net/keys/k/abc123")
	if err != nil || vaultURL != "https://v.vault.azure.net" || key != "k" || version != "abc123" {
		t.Errorf("SplitAzureKeyURI = %q %q %q %v", vaultURL, key, version, err)
	}
	if (keywrap.Spec{}).String() != "" || (keywrap.Spec{}).Enabled() {
		t.Error("the zero Spec must be off")
	}
	if len(keywrap.Providers()) != 4 {
		t.Errorf("Providers = %v", keywrap.Providers())
	}
	for _, p := range append(keywrap.Providers(), "other") {
		if keywrap.Hint(p) == "" {
			t.Errorf("no hint for %s", p)
		}
	}
}
