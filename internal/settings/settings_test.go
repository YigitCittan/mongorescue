package settings

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// memRepo is an in-memory Repository.
type memRepo struct {
	mu     sync.Mutex
	values map[string]string
	saves  int
	fail   error
}

func (m *memRepo) LoadSettings(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.values), nil
}

func (m *memRepo) SaveSettings(_ context.Context, v map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	maps.Copy(m.values, v)
	m.saves++
	return nil
}

func newSvc(t *testing.T, repo *memRepo) *Service {
	t.Helper()
	svc, err := NewService(context.Background(), repo, WithClock(func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func ptr[T any](v T) *T { return &v }

func TestDefaultsAndMaskedJSON(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	raw, err := json.Marshal(svc.Masked())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"backup_timeout":"6h0m0s"`, `"restore_verify_policy":"auto"`, `"session_idle_timeout":"12h0m0s"`,
		`"secure_cookies":"auto"`, `"cors_origins":[]`, `"mcp_enabled":true`, `"identity":""`, `"retired_keys":[]`, `"mode":"x25519"`, `"default_gzip":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("defaults JSON lacks %s: %s", want, raw)
		}
	}
	if svc.Encryptor() != nil || svc.Decryptor() != nil {
		t.Fatal("no keys by default")
	}
}

func TestUpdateValidatesPersistsAndAppliesLive(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	got, err := svc.Update(ctx, Patch{General: &GeneralPatch{BackupTimeout: ptr(Duration(90 * time.Minute)), DefaultRetentionDays: ptr(7)},
		Security: &SecurityPatch{CORSOrigins: ptr([]string{" https://ops.example.com/ ", "https://ops.example.com"})}})
	if err != nil {
		t.Fatal(err)
	}
	if got.General.BackupTimeout.Std() != 90*time.Minute || got.General.DefaultRetentionCount != 10 ||
		len(got.Security.CORSOrigins) != 1 || got.Security.CORSOrigins[0] != "https://ops.example.com" {
		t.Fatalf("updated = %+v", got)
	}
	if svc.Current().General.DefaultRetentionDays != 7 {
		t.Fatal("update not applied live")
	}
	// Only changed keys are written; a reload sees them.
	if len(repo.values) != 3 || repo.values[KeyBackupTimeout] != `"1h30m0s"` {
		t.Fatalf("stored = %v", repo.values)
	}
	again := newSvc(t, repo)
	if again.Current().General.BackupTimeout.Std() != 90*time.Minute {
		t.Fatal("stored setting not loaded")
	}
	// A no-op update does not write.
	saves := repo.saves
	if _, err := svc.Update(ctx, Patch{General: &GeneralPatch{DefaultRetentionDays: ptr(7)}}); err != nil || repo.saves != saves {
		t.Fatalf("no-op update = %v, saves %d -> %d", err, saves, repo.saves)
	}

	for name, p := range map[string]Patch{
		"negative retention": {General: &GeneralPatch{DefaultRetentionDays: ptr(-1)}},
		"negative timeout":   {General: &GeneralPatch{BackupTimeout: ptr(Duration(-time.Second))}},
		"verify policy":      {General: &GeneralPatch{RestoreVerifyPolicy: ptr(models.VerifyPolicy("sometimes"))}},
		"short session":      {Security: &SecurityPatch{SessionIdleTimeout: ptr(Duration(time.Second))}},
		"absolute < idle":    {Security: &SecurityPatch{SessionAbsoluteTimeout: ptr(Duration(time.Hour))}},
		"cookie policy":      {Security: &SecurityPatch{SecureCookies: ptr(CookiePolicy("maybe"))}},
		"cors path":          {Security: &SecurityPatch{CORSOrigins: ptr([]string{"https://a.example.com/path"})}},
		"cors scheme":        {Security: &SecurityPatch{CORSOrigins: ptr([]string{"ftp://a.example.com"})}},
		"cors wildcard":      {Security: &SecurityPatch{CORSOrigins: ptr([]string{"*"})}},
		"mode":               {Encryption: &EncryptionPatch{Mode: ptr(EncryptionMode("rot13"))}},
		"bad recipient":      {Encryption: &EncryptionPatch{Recipients: ptr([]string{"age1nope"})}},
		"bad identity":       {Encryption: &EncryptionPatch{Identity: ptr("AGE-SECRET-KEY-1NOPE")}},
		"short passphrase":   {Encryption: &EncryptionPatch{Passphrase: ptr("too short")}},
		"enabled no keys":    {Encryption: &EncryptionPatch{Enabled: ptr(true)}},
		"passphrase mode":    {Encryption: &EncryptionPatch{Enabled: ptr(true), Mode: ptr(ModePassphrase)}},
		"masked nothing":     {Encryption: &EncryptionPatch{Identity: ptr(SecretMask)}},
	} {
		if _, err := svc.Update(ctx, p); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrMaskedSecret) {
			t.Errorf("%s: %v; want a validation error", name, err)
		}
	}
	if svc.Current().General.DefaultRetentionDays != 7 {
		t.Fatal("a rejected update must not change the settings")
	}
}

func TestDurationJSON(t *testing.T) {
	var p Patch
	if err := json.Unmarshal([]byte(`{"general":{"backup_timeout":"45m"}}`), &p); err != nil || p.General.BackupTimeout.Std() != 45*time.Minute {
		t.Fatalf("decode = %+v, %v", p.General, err)
	}
	for _, bad := range []string{`{"general":{"backup_timeout":3600}}`, `{"general":{"backup_timeout":"soon"}}`} {
		if err := json.Unmarshal([]byte(bad), &p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", bad, err)
		}
	}
}

func TestEncryptionKeysRetireAndKeepDecrypting(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	first, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	masked, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(true), Recipients: ptr([]string{first.Recipient}), Identity: ptr(first.Identity)}})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Encryption.Identity != SecretMask || svc.Encryptor() == nil || svc.Encryptor().Mode() != encryption.ModeX25519 {
		t.Fatalf("after enabling: %+v", masked.Encryption)
	}
	ciphertext := encrypt(t, svc.Encryptor(), "old backup")

	// The masked identity keeps the stored one.
	if _, err = svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Identity: ptr(SecretMask)}}); err != nil || svc.Current().Encryption.Identity != first.Identity {
		t.Fatalf("masked identity: %v", err)
	}
	// A new key pair retires the old identity; it still decrypts the old backup.
	second, _ := GenerateKey()
	masked, err = svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Recipients: ptr([]string{second.Recipient}), Identity: ptr(second.Identity)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(masked.Encryption.RetiredKeys) != 1 || masked.Encryption.RetiredKeys[0].Recipient != first.Recipient || masked.Encryption.RetiredKeys[0].Secret != "" {
		t.Fatalf("retired keys = %+v", masked.Encryption.RetiredKeys)
	}
	raw, _ := json.Marshal(masked)
	if strings.Contains(string(raw), first.Identity) || strings.Contains(string(raw), second.Identity) {
		t.Fatal("masked settings leak an identity")
	}
	decrypt(t, svc.Decryptor(), ciphertext, "old backup")
	// Survives a reload (the retired key is stored).
	reloaded := newSvc(t, repo)
	decrypt(t, reloaded.Decryptor(), ciphertext, "old backup")
	// Removing the identity and disabling encryption still keeps it restorable.
	if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(false), Identity: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if svc.Encryptor() != nil || len(svc.Current().Encryption.RetiredKeys) != 2 {
		t.Fatalf("after removing: %+v", svc.Current().Encryption)
	}
	decrypt(t, svc.Decryptor(), ciphertext, "old backup")
	// Re-activating a retired identity removes it from the retired list.
	if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Identity: ptr(first.Identity)}}); err != nil {
		t.Fatal(err)
	}
	if n := len(svc.Current().Encryption.RetiredKeys); n != 1 {
		t.Fatalf("retired keys after reactivation = %d; want 1", n)
	}
}

func TestPassphraseModeAndRetiredPassphrase(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	ctx := context.Background()
	const pass = "correct horse battery staple"
	if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Enabled: ptr(true), Mode: ptr(ModePassphrase), Passphrase: ptr(pass)}}); err != nil {
		t.Fatal(err)
	}
	if svc.Encryptor().Mode() != encryption.ModeScrypt {
		t.Fatal("passphrase mode must use scrypt")
	}
	ciphertext := encrypt(t, svc.Encryptor(), "scrypt backup")
	if _, err := svc.Update(ctx, Patch{Encryption: &EncryptionPatch{Passphrase: ptr("another long passphrase!")}}); err != nil {
		t.Fatal(err)
	}
	decrypt(t, svc.Decryptor(), ciphertext, "scrypt backup")
}

func TestImportOnceAndMarkers(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	items := []Import{
		{Key: KeyBackupTimeout, Value: "2h", Source: "ENV_A"},
		{Key: KeyRestoreTimeout, Value: "forever", Source: "ENV_B"},
		{Key: KeyEncryptionPassphrase, Value: "short", Source: "ENV_C"}, // legacy passphrases may be short
	}
	res, err := svc.Import(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Imported) != 2 || len(res.Invalid) != 1 || res.Invalid[0] != "ENV_B" {
		t.Fatalf("result = %+v", res)
	}
	if svc.Current().General.BackupTimeout.Std() != 2*time.Hour || svc.Current().Encryption.Passphrase != "short" {
		t.Fatalf("imported = %+v", svc.Current())
	}
	// The short imported passphrase does not block unrelated updates.
	if _, err = svc.Update(ctx, Patch{General: &GeneralPatch{DefaultGzip: ptr(false)}}); err != nil {
		t.Fatalf("update with an imported short passphrase: %v", err)
	}
	// Second start: every source is ignored.
	again := newSvc(t, repo)
	items[0].Value = "3h"
	res, err = again.Import(ctx, items)
	if err != nil || len(res.Imported) != 0 || len(res.Ignored) != 3 || again.Current().General.BackupTimeout.Std() != 2*time.Hour {
		t.Fatalf("second import = %+v, %v", res, err)
	}
	// A setting that already has a stored value is not overwritten.
	fresh := &memRepo{values: map[string]string{KeyDefaultRetentionDays: "3"}}
	s3 := newSvc(t, fresh)
	res, _ = s3.Import(ctx, []Import{{Key: KeyDefaultRetentionDays, Value: 9, Source: "ENV_D"}})
	if s3.Current().General.DefaultRetentionDays != 3 || len(res.Ignored) != 1 || !s3.WasImported("ENV_D") {
		t.Fatalf("stored value overwritten: %+v %+v", s3.Current().General, res)
	}
}

func TestSaveFailureKeepsSnapshot(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	repo.fail = errors.New("disk full")
	if _, err := svc.Update(context.Background(), Patch{General: &GeneralPatch{DefaultRetentionDays: ptr(1)}}); err == nil {
		t.Fatal("save failure must be reported")
	}
	if svc.Current().General.DefaultRetentionDays != 30 {
		t.Fatal("a failed save must not change the live settings")
	}
}

func TestMCPEnabledToggle(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	if !svc.Current().Security.MCPEnabled {
		t.Fatal("the MCP endpoint is enabled by default")
	}
	if _, err := svc.Update(context.Background(), Patch{Security: &SecurityPatch{MCPEnabled: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if svc.Current().Security.MCPEnabled || repo.values[KeyMCPEnabled] != "false" {
		t.Fatalf("mcp_enabled not switched off: %v", repo.values)
	}
	if newSvc(t, repo).Current().Security.MCPEnabled {
		t.Fatal("a reload must keep mcp_enabled off")
	}
}

func TestKeysAndSecrets(t *testing.T) {
	if !IsSecret(KeyEncryptionIdentity) || !IsSecret(KeyEncryptionPassphrase) || !IsSecret(KeyEncryptionRetiredKeys) || IsSecret(KeyBackupTimeout) {
		t.Fatal("secret key classification is wrong")
	}
	if !IsKnown(KeyCORSOrigins) || !IsKnown("legacy_import.X") || IsKnown("secret_key_check") {
		t.Fatal("known key classification is wrong")
	}
	if len(Keys()) != 20 || IsSecret(KeyMCPEnabled) {
		t.Fatalf("Keys() = %d", len(Keys()))
	}
}
