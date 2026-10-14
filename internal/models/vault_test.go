package models

import (
	"errors"
	"testing"
)

func TestVaultRefNormalize(t *testing.T) {
	kv := VaultRef{Engine: " KV-V2 ", Path: "/mongo/prod/"}
	if err := kv.Normalize(VaultForConnection); err != nil {
		t.Fatal(err)
	}
	if kv.Engine != VaultKV2 || kv.Mount != "secret" || kv.Path != "mongo/prod" || kv.UsernameField != "username" || kv.PasswordField != "password" {
		t.Fatalf("kv = %+v", kv)
	}
	s3 := VaultRef{Engine: VaultKV2, Mount: "team/kv", Path: "s3", SessionTokenField: "token"}
	if err := s3.Normalize(VaultForS3); err != nil || s3.AccessKeyField != "access_key" || s3.SecretKeyField != "secret_key" {
		t.Fatalf("s3 = %+v, %v", s3, err)
	}
	db := VaultRef{Engine: VaultDatabase, Role: "backup"}
	if err := db.Normalize(VaultForConnection); err != nil || db.Mount != "database" || !db.Dynamic() {
		t.Fatalf("database = %+v, %v", db, err)
	}
	for name, tc := range map[string]struct {
		ref VaultRef
		use VaultUse
	}{
		"aws for a connection":      {VaultRef{Engine: VaultAWS, Role: "r"}, VaultForConnection},
		"database for s3":           {VaultRef{Engine: VaultDatabase, Role: "r"}, VaultForS3},
		"unknown engine":            {VaultRef{Engine: "kv-v1", Path: "p"}, VaultForConnection},
		"kv without a path":         {VaultRef{Engine: VaultKV2}, VaultForConnection},
		"kv with a role":            {VaultRef{Engine: VaultKV2, Path: "p", Role: "r"}, VaultForConnection},
		"role without a role":       {VaultRef{Engine: VaultDatabase}, VaultForConnection},
		"role with a slash":         {VaultRef{Engine: VaultDatabase, Role: "a/b"}, VaultForConnection},
		"dynamic with a path":       {VaultRef{Engine: VaultDatabase, Role: "r", Path: "p"}, VaultForConnection},
		"dynamic with fields":       {VaultRef{Engine: VaultAWS, Role: "r", AccessKeyField: "a"}, VaultForS3},
		"dot-dot segment":           {VaultRef{Engine: VaultKV2, Path: "a/../sys"}, VaultForConnection},
		"empty segment":             {VaultRef{Engine: VaultKV2, Path: "a//b"}, VaultForConnection},
		"query in a path":           {VaultRef{Engine: VaultKV2, Path: "a?list=true"}, VaultForConnection},
		"system mount":              {VaultRef{Engine: VaultKV2, Mount: "sys", Path: "leases"}, VaultForConnection},
		"auth mount":                {VaultRef{Engine: VaultDatabase, Mount: "auth/token", Role: "r"}, VaultForConnection},
		"s3 fields on a connection": {VaultRef{Engine: VaultKV2, Path: "p", AccessKeyField: "a"}, VaultForConnection},
		"same field twice":          {VaultRef{Engine: VaultKV2, Path: "p", UsernameField: "x", PasswordField: "x"}, VaultForConnection},
		"bad field name":            {VaultRef{Engine: VaultKV2, Path: "p", PasswordField: "pass word"}, VaultForConnection},
	} {
		ref := tc.ref
		if err := ref.Normalize(tc.use); !errors.Is(err, ErrInvalidVaultRef) {
			t.Errorf("%s = %v; want ErrInvalidVaultRef", name, err)
		}
	}
	if _, err := NormalizeCredentialSource("env"); !errors.Is(err, ErrInvalidVaultRef) {
		t.Errorf("unknown source = %v", err)
	}
	if s, err := NormalizeCredentialSource(""); err != nil || s != CredentialsStored {
		t.Errorf("empty source = %q, %v", s, err)
	}
}

func TestSameCredentialsWithVault(t *testing.T) {
	ref := VaultRef{Engine: VaultKV2, Mount: "secret", Path: "s3", AccessKeyField: "access_key", SecretKeyField: "secret_key"}
	a := &StorageTarget{Type: StorageS3, S3: &S3Target{Endpoint: "https://a.example", CredentialSource: CredentialsVault, Vault: ref}}
	b := &StorageTarget{Type: StorageS3, S3: &S3Target{Endpoint: "https://b.example", CredentialSource: CredentialsVault, Vault: ref}}
	if !SameCredentials(a, b) {
		t.Error("two targets reading one Vault secret do not share credentials")
	}
	b.S3.Vault.Path = "other"
	if SameCredentials(a, b) {
		t.Error("two Vault secrets share credentials")
	}
	c := &StorageTarget{Type: StorageS3, S3: &S3Target{Endpoint: "https://a.example"}}
	if SameCredentials(a, c) {
		t.Error("keys from Vault and the default chain share credentials")
	}
}
