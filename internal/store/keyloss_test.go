package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// secretTables returns every row of the tables holding secrets, in a stable form.
func secretTables(t *testing.T, path string) string {
	t.Helper()
	db := rawDB(t, path)
	var out strings.Builder
	for _, q := range []string{
		"SELECT key, value FROM settings ORDER BY key",
		"SELECT id, data FROM connections ORDER BY id",
		"SELECT id, data FROM notification_channels ORDER BY id",
		"SELECT id, data FROM storage_targets ORDER BY id",
		"SELECT id, data FROM jobs ORDER BY id",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "%s|%s\n", k, v)
		}
		_ = rows.Close()
	}
	return out.String()
}

// TestLostSecretKeyLeavesCredentialsIntact opens a database holding every kind of
// sealed credential with a different key (secret.key lost or regenerated, or
// MONGORESCUE_SECRET_KEY changed). The open must fail with ErrSecretKeyMismatch and
// leave every stored value untouched, so that restoring the original key recovers
// them all.
func TestLostSecretKeyLeavesCredentialsIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()

	const uri = "mongodb://backup:conn-password-7@db.internal:27017/"
	if err := s.SaveConnection(ctx, &models.Connection{ID: "conn_1", Name: "prod", URI: uri, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChannel(ctx, &notify.Channel{ID: "ch_1", Name: "tg", Type: notify.ChannelTelegram,
		Telegram: &notify.TelegramConfig{BotToken: "123:bot-token-7", ChatID: "42"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateStorageTarget(ctx, &models.StorageTarget{ID: "tgt_s3", Name: "Offsite", Type: models.StorageS3,
		S3: &models.S3Target{Bucket: "b", AccessKeyID: "AKIA", SecretAccessKey: "s3-secret-7"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{settings.KeyEncryptionPassphrase: `"a long backup passphrase 7"`}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := secretTables(t, path)

	for i := range 3 {
		if _, err := openWith(t, path, storetest.NewBox(t)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
			t.Fatalf("open %d with another key = %v; want ErrSecretKeyMismatch", i, err)
		} else if !strings.Contains(err.Error(), "secret.key") || !strings.Contains(err.Error(), "MONGORESCUE_SECRET_KEY") {
			t.Fatalf("the error must point at the key sources: %v", err)
		}
	}
	if after := secretTables(t, path); after != before {
		t.Fatalf("a refused open changed stored secrets:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The original key recovers everything.
	s2, err := openWith(t, path, testBox)
	if err != nil {
		t.Fatalf("open with the original key: %v", err)
	}
	if c, err := s2.GetConnection(ctx, "conn_1"); err != nil || c.URI != uri {
		t.Fatalf("connection = %+v, %v", c, err)
	}
	if ch, err := s2.GetChannel(ctx, "ch_1"); err != nil || ch.Telegram.BotToken != "123:bot-token-7" {
		t.Fatalf("channel = %+v, %v", ch, err)
	}
	if tgt, err := s2.GetStorageTarget(ctx, "tgt_s3"); err != nil || tgt.S3.SecretAccessKey != "s3-secret-7" {
		t.Fatalf("storage target = %+v, %v", tgt, err)
	}
	if vals, err := s2.LoadSettings(ctx); err != nil || vals[settings.KeyEncryptionPassphrase] != `"a long backup passphrase 7"` {
		t.Fatalf("settings = %v, %v", vals, err)
	}
}

// TestTamperedKeyCheckIsAMismatch damages the stored key check value: the right key
// no longer verifies, and startup refuses rather than re-creating the check value.
func TestTamperedKeyCheckIsAMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"flipped": `UPDATE settings SET value = substr(value, 1, 10) || CASE substr(value, 11, 1) WHEN 'A' THEN 'B' ELSE 'A' END || substr(value, 12) WHERE key = 'secret_key_check'`,
		"planted": `UPDATE settings SET value = 'mongorescue-secret-key-check-v1' WHERE key = 'secret_key_check'`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), dbFile)
			copyDB(t, path, p)
			if _, err := rawDB(t, p).Exec(q); err != nil {
				t.Fatal(err)
			}
			before := secretTables(t, p)
			if _, err := openWith(t, p, testBox); err == nil {
				t.Fatal("a damaged key check value must be refused")
			}
			if after := secretTables(t, p); after != before {
				t.Fatal("a refused open must not rewrite the key check value")
			}
		})
	}
}

// copyDB copies a closed database with VACUUM INTO, which includes its WAL.
func copyDB(t *testing.T, from, to string) {
	t.Helper()
	if _, err := rawDB(t, from).Exec("VACUUM INTO ?", to); err != nil {
		t.Fatal(err)
	}
}
