package store_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// rawData returns every data column of table.
func rawData(t *testing.T, path, table string) string {
	t.Helper()
	rows, err := rawDB(t, path).Query("SELECT data FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		all = append(all, d)
	}
	return strings.Join(all, "\n")
}

func openWith(t *testing.T, path string, box *secretbox.Box) (*store.SQLiteStore, error) {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), store.WithSecretBox(box))
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()

	const uri = "mongodb://backup:conn-password-1@db.internal:27017/"
	if err := s.SaveConnection(ctx, &models.Connection{ID: "conn_1", Name: "prod", URI: uri, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	channels := []*notify.Channel{
		{ID: "c1", Name: "hook", Type: notify.ChannelWebhook, Webhook: &notify.WebhookConfig{
			URL: "https://hooks.example.com/T000/B000/webhook-path-token", Secret: "hmac-secret-1",
			Headers: map[string]string{"Authorization": "Bearer header-token-1"}}},
		{ID: "c2", Name: "tg", Type: notify.ChannelTelegram, Telegram: &notify.TelegramConfig{BotToken: "123:bot-token-1", ChatID: "42"}},
		{ID: "c3", Name: "mail", Type: notify.ChannelEmail, Email: &notify.EmailConfig{Host: "smtp.example.com", Port: 587, Username: "u", Password: "smtp-password-1", From: "a@b.co", To: []string{"c@d.co"}}},
		{ID: "c4", Name: "sms", Type: notify.ChannelTwilio, Twilio: &notify.TwilioConfig{AccountSID: "AC00", AuthToken: "twilio-token-1", From: "+1", To: []string{"+2"}}},
	}
	for _, ch := range channels {
		if err := s.SaveChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}

	raw := rawData(t, path, "connections") + rawData(t, path, "notification_channels")
	for _, secret := range []string{"conn-password-1", "webhook-path-token", "hmac-secret-1", "header-token-1", "bot-token-1", "smtp-password-1", "twilio-token-1"} {
		if strings.Contains(raw, secret) {
			t.Errorf("%q stored in plaintext", secret)
		}
	}
	if !strings.Contains(raw, secretbox.Prefix) {
		t.Fatal("no sealed values found")
	}
	// Non-secret fields stay readable for debugging.
	for _, plain := range []string{"smtp.example.com", `"chat_id":"42"`, "AC00"} {
		if !strings.Contains(raw, plain) {
			t.Errorf("non-secret %q should stay in plaintext", plain)
		}
	}

	c, err := s.GetConnection(ctx, "conn_1")
	if err != nil || c.URI != uri {
		t.Fatalf("GetConnection = %+v, %v", c, err)
	}
	got, _ := s.GetChannel(ctx, "c1")
	if got.Webhook.URL != channels[0].Webhook.URL || got.Webhook.Headers["Authorization"] != "Bearer header-token-1" {
		t.Fatalf("webhook secrets not decrypted: %+v", got.Webhook)
	}
	if err := s.SaveDeliveryStatus(ctx, "c2", notify.DeliveryStatus{Success: true}); err != nil {
		t.Fatal(err)
	}
	if tg, _ := s.GetChannel(ctx, "c2"); tg.Telegram.BotToken != "123:bot-token-1" || tg.LastDelivery == nil {
		t.Fatalf("delivery status must not disturb encrypted fields: %+v", tg)
	}
}

func TestWrongSecretKeyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openWith(t, path, storetest.NewBox(t)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("open with another key = %v; want ErrSecretKeyMismatch", err)
	}
	if _, err := openWith(t, path, testBox); err != nil {
		t.Fatalf("open with the right key: %v", err)
	}
}

func TestTamperedCiphertextIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.SaveConnection(ctx, &models.Connection{ID: "conn_1", Name: "x", URI: "mongodb://u:p@h/", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Flip one character of the sealed URI.
	if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.uri',
		substr(json_extract(data, '$.uri'), 1, 20) || CASE substr(json_extract(data, '$.uri'), 21, 1) WHEN 'A' THEN 'B' ELSE 'A' END ||
		substr(json_extract(data, '$.uri'), 22))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConnection(ctx, "conn_1"); !errors.Is(err, secretbox.ErrDecrypt) && !errors.Is(err, secretbox.ErrMalformed) {
		t.Fatalf("tampered value = %v; want a decryption error", err)
	}
}

func TestPlaintextSecretsAreMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a database written before encryption: plaintext rows and no key check.
	db := rawDB(t, path)
	for _, q := range []string{
		`DELETE FROM settings`,
		`INSERT INTO notification_channels (id, name, type, enabled, data) VALUES ('c1', 'tg', 'telegram', 1,
			'{"id":"c1","name":"tg","type":"telegram","enabled":true,"telegram":{"bot_token":"legacy-bot-token","chat_id":"1"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}')`,
		`INSERT INTO connections (id, name, data) VALUES ('conn_old', 'old',
			'{"id":"conn_old","name":"old","uri":"mongodb://u:legacy-conn-pw@h/","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","last_test_ok":false}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	s2, err := openWith(t, path, testBox)
	if err != nil {
		t.Fatalf("open plaintext database: %v", err)
	}
	raw := rawData(t, path, "notification_channels") + rawData(t, path, "connections")
	if strings.Contains(raw, "legacy-bot-token") || strings.Contains(raw, "legacy-conn-pw") {
		t.Fatalf("plaintext secrets not encrypted on open: %s", raw)
	}
	ctx := context.Background()
	if ch, err := s2.GetChannel(ctx, "c1"); err != nil || ch.Telegram.BotToken != "legacy-bot-token" {
		t.Fatalf("migrated channel = %+v, %v", ch, err)
	}
	if c, err := s2.GetConnection(ctx, "conn_old"); err != nil || c.URI != "mongodb://u:legacy-conn-pw@h/" {
		t.Fatalf("migrated connection = %+v, %v", c, err)
	}
	var kcv int
	if err := rawDB(t, path).QueryRow("SELECT COUNT(*) FROM settings WHERE key = 'secret_key_check'").Scan(&kcv); err != nil || kcv != 1 {
		t.Fatalf("key check value not created: %d, %v", kcv, err)
	}
}

func TestSealedValuesWithoutKeyCheckAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	now := time.Now().UTC()
	if err := s.SaveConnection(context.Background(), &models.Connection{ID: "c", Name: "c", URI: "mongodb://h/", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, path).Exec("DELETE FROM settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := openWith(t, path, storetest.NewBox(t)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("sealed values without key check = %v; want ErrSecretKeyMismatch", err)
	}
}

func TestStoreWithoutBoxRefusesSecrets(t *testing.T) {
	s, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), dbFile), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.SaveChannel(ctx, &notify.Channel{ID: "c", Name: "c", Type: notify.ChannelWebhook}); !errors.Is(err, store.ErrNoSecretBox) {
		t.Fatalf("SaveChannel without box = %v", err)
	}
	if err := s.SaveConnection(ctx, &models.Connection{ID: "c"}); !errors.Is(err, store.ErrNoSecretBox) {
		t.Fatalf("SaveConnection without box = %v", err)
	}
	if _, err := s.MigrateLegacyJobURIs(ctx, ""); !errors.Is(err, store.ErrNoSecretBox) {
		t.Fatalf("MigrateLegacyJobURIs without box = %v", err)
	}
}

func TestConnectionsRepository(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, c := range []*models.Connection{
		{ID: "c2", Name: "beta", URI: "mongodb://b/", CreatedAt: now, UpdatedAt: now},
		{ID: "c1", Name: "alpha", URI: "mongodb://a/", CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.SaveConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListConnections(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "c1" {
		t.Fatalf("ListConnections = %+v, %v; want sorted by name", list, err)
	}
	if _, err := s.GetConnection(ctx, "nope"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("GetConnection(missing) = %v", err)
	}
	if err := s.SaveConnection(ctx, nil); !errors.Is(err, store.ErrInvalidRecord) {
		t.Fatalf("SaveConnection(nil) = %v", err)
	}
	if err := s.DeleteConnection(ctx, "nope"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("DeleteConnection(missing) = %v", err)
	}
	// A job referencing a missing connection does not make "nope" exist.
	if err := s.SaveJob(ctx, &models.Job{ID: "j", Name: "j", ConnectionID: "nope"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnection(ctx, "nope"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("DeleteConnection(missing, referenced) = %v", err)
	}
}

func TestMigrateLegacyJobURIs(t *testing.T) {
	dir := t.TempDir()
	const (
		uriA       = "mongodb://app:pw-a@db-a.internal:27017/shop"
		uriB       = "mongodb://app:pw-b@db-b1.internal:27017,db-b2.internal:27017/?replicaSet=rs"
		defaultURI = "mongodb://root:pw-default@default.internal:27017/"
	)
	statePath := writeState(t, dir, legacyFixture{
		Jobs: map[string]*fixtureJob{
			"job_a1": {Job: models.Job{ID: "job_a1", Name: "a1", Database: "shop"}, MongoURI: uriA},
			"job_a2": {Job: models.Job{ID: "job_a2", Name: "a2", Database: "crm"}, MongoURI: uriA},
			"job_b":  {Job: models.Job{ID: "job_b", Name: "b", Database: "logs"}, MongoURI: uriB},
			"job_d":  {Job: models.Job{ID: "job_d", Name: "d", Database: "misc"}},
		},
		Backups: map[string]*models.BackupRecord{
			"bkp_1": {ID: "bkp_1", JobID: "job_a1", Database: "shop", Status: models.StatusCompleted},
			"bkp_2": {ID: "bkp_2", Database: "shop", Status: models.StatusCompleted},
		},
	})
	path := filepath.Join(dir, dbFile)
	s, _ := openLogged(t, path)
	ctx := context.Background()
	if _, err := s.MigrateLegacyState(ctx, statePath); err != nil {
		t.Fatal(err)
	}

	res, err := s.MigrateLegacyJobURIs(ctx, defaultURI)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConnectionsCreated != 3 || res.JobsAssigned != 4 || res.JobsUnassigned != 0 {
		t.Fatalf("migration = %+v", res)
	}
	conns, _ := s.ListConnections(ctx)
	byURI := map[string]*models.Connection{}
	for _, c := range conns {
		byURI[c.URI] = c
	}
	if byURI[uriA] == nil || byURI[uriA].Name != "db-a.internal:27017" ||
		byURI[uriB] == nil || byURI[uriB].Name != "db-b1.internal:27017,db-b2.internal:27017" ||
		byURI[defaultURI] == nil || byURI[defaultURI].Name != "default" {
		t.Fatalf("connections = %+v", conns)
	}
	for id, want := range map[string]string{"job_a1": uriA, "job_a2": uriA, "job_b": uriB, "job_d": defaultURI} {
		j, _ := s.GetJob(ctx, id)
		if j.ConnectionID != byURI[want].ID {
			t.Errorf("%s connection = %q; want %q", id, j.ConnectionID, byURI[want].ID)
		}
	}
	if raw := rawData(t, path, "jobs"); strings.Contains(raw, "mongo_uri") || strings.Contains(raw, "pw-a") {
		t.Fatalf("legacy uri left in job data: %s", raw)
	}
	b1, _ := s.GetBackupRecord(ctx, "bkp_1")
	b2, _ := s.GetBackupRecord(ctx, "bkp_2")
	if b1.ConnectionID != byURI[uriA].ID || b1.ConnectionName != "db-a.internal:27017" || b2.ConnectionID != "" {
		t.Fatalf("backup records = %+v / %+v", b1, b2)
	}
	if again, err := s.MigrateLegacyJobURIs(ctx, defaultURI); err != nil || again != (store.LegacyURIMigration{}) {
		t.Fatalf("second migration = %+v, %v; want no-op", again, err)
	}
}

func TestMigrateLegacyJobURIsWithoutDefault(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	if err := s.SaveJob(ctx, &models.Job{ID: "orphan", Name: "o", Database: "d"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.MigrateLegacyJobURIs(ctx, "")
	if err != nil || res.JobsUnassigned != 1 || res.ConnectionsCreated != 0 {
		t.Fatalf("migration = %+v, %v", res, err)
	}
	if j, _ := s.GetJob(ctx, "orphan"); j.ConnectionID != "" {
		t.Fatalf("job without any uri must stay unassigned: %+v", j)
	}
}

// sealLegacy produces an "sb1:" value with testKey the way earlier builds did (the
// associated data was only the version byte).
func sealLegacy(t *testing.T, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	buf := append([]byte{1}, make([]byte, aead.NonceSize())...)
	return secretbox.LegacyPrefix + base64.StdEncoding.EncodeToString(aead.Seal(buf, buf[1:], []byte(plain), []byte{1}))
}

func TestSecretsThatLookSealedAreStillEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	lookalikes := []string{"sb1:attacker-controlled", secretbox.Prefix + "AAAA", sealLegacy(t, "x")}
	for i, v := range lookalikes {
		ch := &notify.Channel{ID: "c" + string(rune('0'+i)), Name: "tg", Type: notify.ChannelTelegram,
			Telegram: &notify.TelegramConfig{BotToken: v, ChatID: "1"}}
		if err := s.SaveChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	raw := rawData(t, path, "notification_channels")
	for _, v := range lookalikes {
		if strings.Contains(raw, v) {
			t.Errorf("%q stored as is instead of encrypted", v)
		}
	}
	list, err := s.ListChannels(ctx)
	if err != nil || len(list) != len(lookalikes) {
		t.Fatalf("ListChannels = %d, %v; want every channel readable", len(list), err)
	}
	for i, v := range lookalikes {
		if ch, err := s.GetChannel(ctx, "c"+string(rune('0'+i))); err != nil || ch.Telegram.BotToken != v {
			t.Fatalf("channel %d = %+v, %v; want token %q", i, ch, err, v)
		}
	}
}

func TestPrefixInNonSecretFieldsDoesNotBlockStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.SaveJob(ctx, &models.Job{ID: "j", Name: "sb1: nightly", Database: "sb1:db"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChannel(ctx, &notify.Channel{ID: "c", Name: "sb1:alerts", Type: notify.ChannelEmail,
		Email: &notify.EmailConfig{Host: "sb1:smtp", Port: 25, From: "a@b.co", To: []string{"c@d.co"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConnection(ctx, &models.Connection{ID: "con", Name: "sb2:prod", Description: "sb1:", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Without a key check value (an upgrade from a build that had none) the database
	// holds no sealed secret, so a new key is accepted.
	if _, err := rawDB(t, path).Exec("DELETE FROM settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := openWith(t, path, storetest.NewBox(t)); err != nil {
		t.Fatalf("names containing a sealed prefix must not look like secrets: %v", err)
	}
}

func TestCiphertextsCannotBeSwappedBetweenRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, c := range []*models.Connection{
		{ID: "staging", Name: "staging", URI: "mongodb://u:staging-pw@staging/", CreatedAt: now, UpdatedAt: now},
		{ID: "prod", Name: "prod", URI: "mongodb://u:prod-pw@prod/", CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.SaveConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Someone with write access to the database copies prod's ciphertext into staging.
	if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.uri',
		(SELECT json_extract(data, '$.uri') FROM connections WHERE id = 'prod')) WHERE id = 'staging'`); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetConnection(ctx, "staging"); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Fatalf("swapped ciphertext = %+v, %v; want ErrDecrypt", c, err)
	}
	// Moving a channel secret into another field fails the same way.
	if err := s.SaveChannel(ctx, &notify.Channel{ID: "w", Name: "w", Type: notify.ChannelWebhook,
		Webhook: &notify.WebhookConfig{URL: "https://h/token", Secret: "hmac"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, path).Exec(`UPDATE notification_channels SET data = json_set(data, '$.webhook.url',
		json_extract(data, '$.webhook.secret')) WHERE id = 'w'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChannel(ctx, "w"); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Fatalf("secret moved to another field = %v; want ErrDecrypt", err)
	}
}

func TestPlaintextSecretsAreRefusedAfterMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.SaveConnection(ctx, &models.Connection{ID: "c", Name: "c", URI: "mongodb://h/", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, planted := range []string{"mongodb://attacker/", sealLegacy(t, "mongodb://attacker/")} {
		if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.uri', ?)`, planted); err != nil {
			t.Fatal(err)
		}
		if c, err := s.GetConnection(ctx, "c"); !errors.Is(err, store.ErrUnsealedSecret) {
			t.Fatalf("planted %q = %+v, %v; want ErrUnsealedSecret", planted, c, err)
		}
	}
}

func TestLegacySealedValuesAreUpgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// A database from a build before the format marker existed.
		{`DELETE FROM settings WHERE key = 'secrets_format'`, nil},
		{`UPDATE settings SET value = ? WHERE key = 'secret_key_check'`, []any{sealLegacy(t, "mongorescue-secret-key-check-v1")}},
		{`INSERT INTO connections (id, name, data) VALUES ('old', 'old', json_object('id', 'old', 'name', 'old', 'uri', ?,
			'created_at', '2026-01-01T00:00:00Z', 'updated_at', '2026-01-01T00:00:00Z'))`, []any{sealLegacy(t, "mongodb://u:legacy-pw@h/")}},
		{`INSERT INTO notification_channels (id, name, type, enabled, data) VALUES ('w', 'w', 'webhook', 1,
			json_object('id', 'w', 'name', 'w', 'type', 'webhook', 'enabled', json('true'), 'webhook',
			json_object('url', ?, 'headers', json_object('X-Token', ?)), 'created_at', '2026-01-01T00:00:00Z', 'updated_at', '2026-01-01T00:00:00Z'))`,
			[]any{sealLegacy(t, "https://h/legacy-token"), sealLegacy(t, "legacy-header")}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}

	s2, err := openWith(t, path, testBox)
	if err != nil {
		t.Fatalf("open a database with legacy values: %v", err)
	}
	raw := rawData(t, path, "connections") + rawData(t, path, "notification_channels")
	if strings.Contains(raw, secretbox.LegacyPrefix) || !strings.Contains(raw, secretbox.Prefix) {
		t.Fatalf("legacy values not re-sealed: %s", raw)
	}
	var kcv string
	if err := rawDB(t, path).QueryRow("SELECT value FROM settings WHERE key = 'secret_key_check'").Scan(&kcv); err != nil || !secretbox.IsSealed(kcv) {
		t.Fatalf("key check value = %q, %v; want the current format", kcv, err)
	}
	ctx := context.Background()
	if c, err := s2.GetConnection(ctx, "old"); err != nil || c.URI != "mongodb://u:legacy-pw@h/" {
		t.Fatalf("upgraded connection = %+v, %v", c, err)
	}
	if ch, err := s2.GetChannel(ctx, "w"); err != nil || ch.Webhook.URL != "https://h/legacy-token" || ch.Webhook.Headers["X-Token"] != "legacy-header" {
		t.Fatalf("upgraded channel = %+v, %v", ch, err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// A legacy check value sealed with another key is still a mismatch.
	if _, err := openWith(t, path, storetest.NewBox(t)); !errors.Is(err, secretbox.ErrSecretKeyMismatch) {
		t.Fatalf("other key after upgrade = %v; want ErrSecretKeyMismatch", err)
	}
}

func TestPlantedSecretsAreRefusedAtStartupOnceUpgraded(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	for name, plant := range map[string]struct {
		query string
		args  []any
		want  string
	}{
		"plaintext uri": {`UPDATE connections SET data = json_set(data, '$.uri', ?)`, []any{"mongodb://attacker:pw@evil.example/"}, "connections|c|uri"},
		"sb1 uri":       {`UPDATE connections SET data = json_set(data, '$.uri', ?)`, []any{sealLegacy(t, "mongodb://attacker:pw@evil.example/")}, "connections|c|uri"},
		"plaintext header": {`UPDATE notification_channels SET data = json_set(data, '$.webhook.headers.X-Token', ?)`, []any{"planted"},
			"notification_channels|w|webhook.headers.X-Token"},
		"plaintext s3 key": {`UPDATE storage_targets SET data = json_set(data, '$.s3.secret_access_key', ?)`, []any{"planted"},
			"storage_targets|stg|s3.secret_access_key"},
		"plaintext legacy job uri": {`UPDATE jobs SET data = json_set(data, '$.mongo_uri', ?)`, []any{"mongodb://evil/"}, "jobs|j|mongo_uri"},
		"sb1 key check": {`UPDATE settings SET value = ? WHERE key = 'secret_key_check'`, []any{sealLegacy(t, "mongorescue-secret-key-check-v1")},
			"settings|secret_key_check|value"},
		"plaintext passphrase": {`INSERT INTO settings (key, value) VALUES ('encryption.passphrase', ?)`, []any{`"planted passphrase"`},
			"settings|encryption.passphrase|value"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), dbFile)
			s := storetest.OpenWithBox(t, path, testBox)
			if err := s.SaveConnection(ctx, &models.Connection{ID: "c", Name: "c", URI: "mongodb://h/", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveChannel(ctx, &notify.Channel{ID: "w", Name: "w", Type: notify.ChannelWebhook,
				Webhook: &notify.WebhookConfig{URL: "https://h/x", Headers: map[string]string{"X-Token": "t"}}}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateStorageTarget(ctx, &models.StorageTarget{ID: "stg", Name: "s", Type: models.StorageS3, CreatedAt: now, UpdatedAt: now,
				S3: &models.S3Target{Bucket: "bkt", AccessKeyID: "a", SecretAccessKey: "b"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveJob(ctx, &models.Job{ID: "j", Name: "j"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := rawDB(t, path).Exec(plant.query, plant.args...); err != nil {
				t.Fatal(err)
			}
			_, err := openWith(t, path, testBox)
			if !errors.Is(err, store.ErrUnsealedSecret) || !strings.Contains(err.Error(), plant.want) {
				t.Fatalf("open with a planted secret = %v; want ErrUnsealedSecret naming %s", err, plant.want)
			}
			if strings.Contains(fmt.Sprint(err), "attacker") || strings.Contains(fmt.Sprint(err), "planted") {
				t.Fatalf("the error must not echo the planted value: %v", err)
			}
		})
	}
}
