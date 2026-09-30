package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// roundTrip encrypts plaintext with the application's current encryptor and decrypts
// it with its current decryptor.
func roundTrip(t *testing.T, a *App, plaintext string) {
	t.Helper()
	enc, dec := a.settings.Encryptor(), a.settings.Decryptor()
	if enc == nil || dec == nil {
		t.Fatalf("encryptor %v, decryptor %v; want both", enc, dec)
	}
	var ct bytes.Buffer
	w, err := enc.Encrypt(&ct)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, plaintext)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := dec.Decrypt(&ct)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || string(got) != plaintext {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

func connectionsOf(t *testing.T, a *App) []*models.Connection {
	t.Helper()
	list, err := a.metaStore.(*store.SQLiteStore).ListConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestAppImportsDeprecatedEncryptionConnectionAndS3 covers the deprecated variables
// for backup encryption (with an identity file as saved by a Windows editor), the
// MongoDB connection string and S3 storage, as docs/configuration.md documents them.
func TestAppImportsDeprecatedEncryptionConnectionAndS3(t *testing.T) {
	cfg := testConfig(t)
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	_, second, _ := encryption.GenerateX25519()
	idFile := filepath.Join(t.TempDir(), "age.key")
	// UTF-8 byte order mark and CRLF line endings.
	if err = os.WriteFile(idFile, []byte("\ufeff# created: 2026-01-01\r\n"+identity+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const uri = "mongodb://legacy:legacy-uri-pass@mongo.internal:27017/?authSource=admin"
	env := map[string]string{
		config.EnvEncryptionEnabled: "true",
		config.EnvEncryptionRecips:  recipient + ", " + second,
		config.EnvEncryptionIDFile:  idFile,
		config.EnvMongoURI:          uri,
		config.EnvStorageType:       "s3",
		config.EnvS3Bucket:          "legacy-bucket",
		config.EnvS3Endpoint:        "https://minio.internal:9000",
		config.EnvS3PathStyle:       "true",
		"AWS_REGION":                "eu-west-1",
		"AWS_ACCESS_KEY_ID":         "AKIALEGACY",
		"AWS_SECRET_ACCESS_KEY":     "legacy-s3-secret",
	}
	var logs strings.Builder
	a, err := New(cfg, slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)), WithGetenv(envMap(env)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()

	cur := a.settings.Current().Encryption
	if !cur.Enabled || cur.Mode != settings.ModeX25519 || len(cur.Recipients) != 2 || !strings.Contains(cur.Identity, identity) {
		t.Fatalf("imported encryption = %+v", cur)
	}
	roundTrip(t, a, "backup written after the import")

	conns := connectionsOf(t, a)
	if len(conns) != 1 || conns[0].Name != "default" || conns[0].URI != uri {
		t.Fatalf("connections = %+v; want the imported default", conns)
	}
	def, err := a.targets.Resolve(ctx, "")
	if err != nil || def.S3 == nil || def.S3.Bucket != "legacy-bucket" || def.S3.Endpoint != "https://minio.internal:9000" ||
		!def.S3.UsePathStyle || def.S3.Region != "eu-west-1" || def.S3.AccessKeyID != "AKIALEGACY" || def.S3.SecretAccessKey != "legacy-s3-secret" {
		t.Fatalf("default target = %+v, %v; want the imported S3 storage", def, err)
	}
	out := logs.String()
	for _, secret := range []string{identity, "legacy-uri-pass", "legacy-s3-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("a deprecated secret was logged")
		}
	}
}

// TestAppImportsLegacyPassphraseAndSecretKeyFromConfigFile covers config.json: a
// short passphrase from an earlier build is accepted, and its secret_key is used as
// the key for credentials at rest (no secret.key is generated) when
// MONGORESCUE_SECRET_KEY is not set.
func TestAppImportsLegacyPassphraseAndSecretKeyFromConfigFile(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	key, _ := secretbox.GenerateKey()
	file := `{"secret_key": "` + secretbox.EncodeKey(key) + `",
		"encryption": {"enabled": true, "passphrase": "short-pass"},
		"mongo": {"uri": "mongodb://file:file-uri-pass@db.internal:27017/"},
		"storage": {"type": "local", "local": {"path": "` + filepath.ToSlash(filepath.Join(t.TempDir(), "legacy")) + `"}}}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, config.LegacyFileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	cur := first.settings.Current().Encryption
	if !cur.Enabled || cur.Mode != settings.ModePassphrase || cur.Passphrase != "short-pass" {
		t.Fatalf("imported encryption = %+v", cur)
	}
	if conns := connectionsOf(t, first); len(conns) != 1 || conns[0].URI != "mongodb://file:file-uri-pass@db.internal:27017/" {
		t.Fatalf("connections = %+v", conns)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, secretbox.KeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("secret.key must not be generated while config.json provides secret_key: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	// Moving the key to MONGORESCUE_SECRET_KEY, as the warning asks, keeps working.
	if err = os.Remove(filepath.Join(cfg.DataDir, config.LegacyFileName)); err != nil {
		t.Fatal(err)
	}
	cfg.SecretKey = secretbox.EncodeKey(key)
	second, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("start with the key moved to the environment: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if conns := connectionsOf(t, second); len(conns) != 1 || conns[0].URI != "mongodb://file:file-uri-pass@db.internal:27017/" {
		t.Fatalf("connections after moving the key = %+v", conns)
	}
	if second.settings.Current().Encryption.Passphrase != "short-pass" {
		t.Fatal("the imported passphrase must survive a restart")
	}
}
