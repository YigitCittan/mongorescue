package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnvAndValidate(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil || cfg.DataDir != DefaultDataDir || cfg.Host != DefaultHost || cfg.Port != DefaultPort || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	key, _ := secretbox.GenerateKey()
	cfg, err = FromEnv(env(map[string]string{EnvDataDir: "/data/", EnvHost: "127.0.0.1", EnvPort: "9000", EnvSecretKey: secretbox.EncodeKey(key)}))
	if err != nil || cfg.DataDir != filepath.FromSlash("/data") || cfg.Host != "127.0.0.1" || cfg.Port != 9000 || cfg.Validate() != nil {
		t.Fatalf("env = %+v, %v", cfg, err)
	}
	if cfg.MetadataDBPath() != filepath.Join(filepath.FromSlash("/data"), DefaultDatabaseFileName) {
		t.Fatalf("db path = %s", cfg.MetadataDBPath())
	}
	wantBackups, _ := filepath.Abs(filepath.FromSlash("/" + DefaultBackupsDirName))
	if dir, _ := cfg.DefaultBackupsDir(); dir != wantBackups {
		t.Fatalf("default backups dir = %s; want /backups next to /data", dir)
	}
	if _, err := FromEnv(env(map[string]string{EnvPort: "http"})); !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("bad port = %v", err)
	}
	for _, c := range []Config{{DataDir: "", Port: 1}, {DataDir: "d", Port: 70000}, {DataDir: "d", Port: 1, Host: "a b"}, {DataDir: "d", Port: 1, SecretKey: "short"}} {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil", c)
		} else if strings.Contains(err.Error(), "short") {
			t.Errorf("error echoes the secret key: %v", err)
		}
	}
	for in, want := range map[string]slog.Level{"DEBUG": slog.LevelDebug, "": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := ParseLogLevel(in); err != nil || got != want {
			t.Errorf("ParseLogLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLogLevel("loud"); !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatal("unknown log level accepted")
	}
}

func settingValue(l *Legacy, key string) (any, string, bool) {
	for _, it := range l.Settings {
		if it.Key == key {
			return it.Value, it.Source, true
		}
	}
	return nil, "", false
}

func TestLoadLegacyEnvironment(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "identity.txt")
	if err := os.WriteFile(idFile, []byte("# comment\nAGE-SECRET-KEY-1FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLegacy(dir, env(map[string]string{
		EnvAPIKey: "legacy-key-0123456789", EnvMongoURI: "mongodb://u:p@h/", EnvBackupTimeout: "2h",
		EnvSecureCookies: "true", EnvTrustProxy: "yes-please", EnvCORSOrigins: "https://a.example.com, ,https://b.example.com",
		EnvEncryptionEnabled: "true", EnvEncryptionPass: "a passphrase", EnvEncryptionIDFile: idFile,
		EnvStorageType: "s3", EnvS3Bucket: "bkt", EnvS3Endpoint: "http://minio:9000", EnvS3PathStyle: "1",
		"AWS_REGION": "eu-west-1", "AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if l.APIKey != "legacy-key-0123456789" || l.APIKeySource != EnvAPIKey || l.MongoURI != "mongodb://u:p@h/" {
		t.Fatalf("legacy = %+v", l)
	}
	if v, src, _ := settingValue(l, settings.KeyBackupTimeout); v != "2h" || src != EnvBackupTimeout {
		t.Fatalf("backup timeout = %v from %s", v, src)
	}
	if v, _, _ := settingValue(l, settings.KeySecureCookies); v != settings.CookiesAlways {
		t.Fatalf("secure cookies = %v", v)
	}
	if v, _, _ := settingValue(l, settings.KeyCORSOrigins); len(v.([]string)) != 2 {
		t.Fatalf("cors = %v", v)
	}
	if v, src, _ := settingValue(l, settings.KeyEncryptionIdentity); v != "# comment\nAGE-SECRET-KEY-1FILE" || src != EnvEncryptionIDFile {
		t.Fatalf("identity = %q from %s", v, src)
	}
	if v, _, _ := settingValue(l, settings.KeyEncryptionMode); v != settings.ModePassphrase {
		t.Fatalf("mode = %v; want passphrase (no recipients)", v)
	}
	if last := l.Settings[len(l.Settings)-1]; last.Key != settings.KeyEncryptionEnabled {
		t.Fatalf("the enable switch must be imported last, got %s", last.Key)
	}
	if _, _, ok := settingValue(l, settings.KeyTrustProxyHeaders); ok || len(l.Problems) != 1 || !slices.Contains(l.Present, EnvTrustProxy) {
		t.Fatalf("invalid boolean must be a problem: %+v / %v", l.Problems, l.Present)
	}
	s3 := l.Storage.Target.S3
	if l.Storage.Target.Type != models.StorageS3 || s3.Bucket != "bkt" || s3.Region != "eu-west-1" || s3.AccessKeyID != "AK" ||
		s3.SecretAccessKey != "SK" || !s3.UsePathStyle || s3.Endpoint != "http://minio:9000" {
		t.Fatalf("storage = %+v", s3)
	}
	for _, std := range []string{"AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if slices.Contains(l.Present, std) {
			t.Errorf("standard AWS variable %s must not be reported as deprecated", std)
		}
	}
	// Standard AWS variables alone are not a storage configuration.
	only, _ := LoadLegacy(t.TempDir(), env(map[string]string{"AWS_REGION": "x", "AWS_ACCESS_KEY_ID": "y"}))
	if only.Storage != nil || len(only.Present) != 0 {
		t.Fatalf("AWS variables alone = %+v", only)
	}
	if got := DeprecatedEnvSet(env(map[string]string{EnvDBPath: "/x", "AWS_REGION": "y"})); !slices.Equal(got, []string{EnvDBPath}) {
		t.Fatalf("DeprecatedEnvSet = %v", got)
	}
}

func TestLoadLegacyFile(t *testing.T) {
	dir := t.TempDir()
	file := `{"server": {"api_key": "file-key-0123456789", "secure_cookies": false, "metrics_public": true},
		"storage": {"type": "local", "local": {"path": "/srv/backups"}},
		"defaults": {"retention_days": 7, "gzip": false},
		"backup": {"timeout": 3600000000000}, "restore": {"verify_before_restore": "ALWAYS"},
		"encryption": {"enabled": true, "recipients": ["age1x"]}, "database_path": "/old/db", "secret_key": "k"}`
	if err := os.WriteFile(filepath.Join(dir, LegacyFileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLegacy(dir, env(map[string]string{EnvBackupTimeout: "3h"}))
	if err != nil {
		t.Fatal(err)
	}
	if l.File == "" || l.APIKey != "file-key-0123456789" || l.DBPath != "/old/db" || l.SecretKey != "k" {
		t.Fatalf("legacy = %+v", l)
	}
	// Environment variables override the file, as before.
	if v, src, _ := settingValue(l, settings.KeyBackupTimeout); v != "3h" || src != EnvBackupTimeout {
		t.Fatalf("backup timeout = %v from %s", v, src)
	}
	if v, src, _ := settingValue(l, settings.KeyDefaultRetentionDays); v != 7 || src != "config.json:defaults.retention_days" {
		t.Fatalf("retention = %v from %s", v, src)
	}
	if v, _, _ := settingValue(l, settings.KeyRestoreVerifyPolicy); v != "always" {
		t.Fatalf("verify = %v", v)
	}
	if v, _, _ := settingValue(l, settings.KeyEncryptionMode); v != settings.ModeX25519 {
		t.Fatalf("mode = %v", v)
	}
	if l.Storage == nil || l.Storage.Target.Local.Path != "/srv/backups" {
		t.Fatalf("storage = %+v", l.Storage)
	}
	if err := os.WriteFile(filepath.Join(dir, LegacyFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLegacy(dir, env(nil)); err == nil {
		t.Fatal("a broken legacy file must be reported")
	}
}
