package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// Upgrade compatibility of the metadata database.
//
// For every schema version N, a fixture database is built the way the releases at
// that version left it: migrations 0001..N are applied one at a time and, after each
// migration, the rows that release wrote (with only the columns that existed then)
// are inserted. Opening the fixture with this build must migrate it to the latest
// version and preserve every row and field. Secrets are sealed as released builds
// sealed them (secretbox "sb2", bound to their location, with a key check value and
// the secrets format marker).

// compatStep seeds the rows written at one schema version and checks them after the
// upgrade.
type compatStep struct {
	version int
	seed    func(t *testing.T, f *compatFixture)
	check   func(t *testing.T, f *compatFixture, s *SQLiteStore)
}

// compatFixture carries the database under construction and the keys its secrets
// and backups were encrypted with.
type compatFixture struct {
	db          *sql.DB
	box         *secretbox.Box
	ageIdentity string
	ageRecip    string
	oldBackupCT []byte
}

var compatT0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func (f *compatFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatalf("seed %q: %v", strings.Fields(query)[0:3], err)
	}
}

func (f *compatFixture) seal(t *testing.T, table, id, field, plain string) string {
	t.Helper()
	v, err := f.box.Seal(secretbox.At(table, id, field), plain)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// jsonDoc marshals v for a data column, failing the test on error.
func jsonDoc(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func ns(d time.Duration) int64 { return compatT0.Add(d).UnixNano() }

func rfc(d time.Duration) string { return compatT0.Add(d).Format(time.RFC3339Nano) }

const (
	compatLegacyJobURI = "mongodb://legacy:legacy-pass-1@old.internal:27017/?authSource=admin"
	compatConnURI      = "mongodb://backup:conn-pass-2@db1.internal:27017,db2.internal:27017/?replicaSet=rs0"
	compatS3Secret     = "s3-secret-access-key-3"
	compatHookSecret   = "hmac-secret-4"
	compatHookURL      = "https://hooks.example.com/T000/B000/path-token-5"
	compatPassword     = "$2a$10$abcdefghijklmnopqrstuuDk0yF6yZrQn5Cq0I2e8fQ1Q3m2m7mJe"
)

var compatSteps = []compatStep{
	{
		version: 1,
		seed: func(t *testing.T, f *compatFixture) {
			// Before secretbox (schema 0001): the job carries its connection string and
			// channel secrets are stored in plaintext.
			f.exec(t, `INSERT INTO jobs (id, name, database_name, enabled, created_at, data) VALUES (?, ?, ?, ?, ?, ?)`,
				"job_v1", "nightly shop", "shop", 1, ns(0), jsonDoc(t, map[string]any{
					"id": "job_v1", "name": "nightly shop", "cron_expression": "0 3 * * *", "database": "shop",
					"collections": []string{"orders"}, "storage_type": "local", "retention_days": 7, "retention_count": 3,
					"gzip": true, "enabled": true, "created_at": rfc(0), "updated_at": rfc(time.Minute),
					"mongo_uri": compatLegacyJobURI,
				}))

			backups := []map[string]any{
				{"id": "bkp_v1_plain", "job_id": "job_v1", "database": "shop", "status": "completed", "storage_type": "local",
					"storage_key": "shop/2026/09/bkp_v1_plain.archive.gz", "size_bytes": 1234, "sha256": strings.Repeat("ab", 32),
					"collections": []string{"orders"}, "started_at": rfc(0), "completed_at": rfc(time.Minute), "duration_seconds": 60.5},
				{"id": "bkp_v1_enc", "database": "shop", "status": "completed", "storage_type": "local",
					"storage_key": "shop/2026/09/bkp_v1_enc.archive.gz.age", "size_bytes": len(f.oldBackupCT),
					"sha256": strings.Repeat("cd", 32), "encrypted": true, "encryption_mode": "x25519",
					"started_at": rfc(time.Hour), "completed_at": rfc(time.Hour + time.Minute)},
				{"id": "bkp_v1_failed", "job_id": "job_v1", "database": "shop", "status": "failed", "storage_type": "local",
					"storage_key": "shop/2026/09/bkp_v1_failed.archive", "error_message": "mongodump failed: exit status 1",
					"started_at": rfc(2 * time.Hour)},
			}
			for _, b := range backups {
				started, _ := time.Parse(time.RFC3339Nano, b["started_at"].(string))
				jobID, _ := b["job_id"].(string)
				f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, data) VALUES (?, ?, ?, ?, ?, ?)`,
					b["id"], jobID, b["database"], b["status"], started.UnixNano(), jsonDoc(t, b))
			}

			f.exec(t, `INSERT INTO restores (id, backup_id, source_database, target_database, status, started_at, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				"rst_v1", "bkp_v1_plain", "shop", "shop_rescue_20260925_130000", "completed", ns(3*time.Hour), jsonDoc(t, map[string]any{
					"id": "rst_v1", "backup_id": "bkp_v1_plain", "source_database": "shop", "target_database": "shop_rescue_20260925_130000",
					"status": "completed", "started_at": rfc(3 * time.Hour), "completed_at": rfc(3*time.Hour + time.Minute), "verified": true,
				}))

			f.exec(t, `INSERT INTO notification_channels (id, name, type, enabled, data) VALUES (?, ?, ?, ?, ?)`,
				"ch_v1", "ops hook", "webhook", 1, jsonDoc(t, map[string]any{
					"id": "ch_v1", "name": "ops hook", "type": "webhook", "enabled": true,
					"webhook": map[string]any{
						"url": compatHookURL, "secret": compatHookSecret, "headers": map[string]string{"X-Token": "header-token-6"},
					},
					"created_at": rfc(0), "updated_at": rfc(0),
				}))
			f.exec(t, `INSERT INTO notification_rules (id, name, enabled, data) VALUES (?, ?, ?, ?)`,
				"rule_v1", "failures", 1, jsonDoc(t, map[string]any{
					"id": "rule_v1", "name": "failures", "enabled": true, "events": []string{"backup.failed", "restore.failed"},
					"job_ids": []string{"job_v1"}, "channel_ids": []string{"ch_v1"}, "created_at": rfc(0), "updated_at": rfc(0),
				}))
		},
		check: func(t *testing.T, f *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			job, err := s.GetJob(ctx, "job_v1")
			if err != nil {
				t.Fatal(err)
			}
			if job.Name != "nightly shop" || job.CronExpression != "0 3 * * *" || job.Database != "shop" || !job.Gzip || !job.Enabled ||
				job.RetentionDays != 7 || job.RetentionCount != 3 || !slices.Equal(job.Collections, []string{"orders"}) ||
				!job.CreatedAt.Equal(compatT0) || !job.UpdatedAt.Equal(compatT0.Add(time.Minute)) {
				t.Errorf("job_v1 = %+v", job)
			}
			// 0013: every job written before database selections backs up its one
			// database as a single selection.
			if sel := job.DatabaseSelection; sel.Mode != models.SelectionSingle || !slices.Equal(sel.Databases, []string{"shop"}) ||
				sel.AutoIncludeNew || job.MultiDatabase() || job.KnownDatabases != nil {
				t.Errorf("job_v1 selection = %+v, known %v", sel, job.KnownDatabases)
			}
			// The legacy job URI becomes a managed connection, as the app does at startup.
			if _, err = s.MigrateLegacyJobURIs(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if job, err = s.GetJob(ctx, "job_v1"); err != nil || job.ConnectionID == "" {
				t.Fatalf("job_v1 after the URI migration = %+v, %v", job, err)
			}
			if conn, connErr := s.GetConnection(ctx, job.ConnectionID); connErr != nil || conn.URI != compatLegacyJobURI {
				t.Fatalf("connection of job_v1 = %+v, %v", conn, connErr)
			}

			plain, err := s.GetBackupRecord(ctx, "bkp_v1_plain")
			if err != nil {
				t.Fatal(err)
			}
			if plain.JobID != "job_v1" || plain.Status != models.StatusCompleted || plain.StorageKey != "shop/2026/09/bkp_v1_plain.archive.gz" ||
				plain.SizeBytes != 1234 || plain.SHA256 != strings.Repeat("ab", 32) || plain.Encrypted || plain.DurationSeconds != 60.5 ||
				plain.Trigger != models.TriggerScheduled || plain.RetryOf != "" || !plain.StartedAt.Equal(compatT0) ||
				plain.CompletedAt == nil || !plain.CompletedAt.Equal(compatT0.Add(time.Minute)) {
				t.Errorf("bkp_v1_plain = %+v", plain)
			}
			enc, err := s.GetBackupRecord(ctx, "bkp_v1_enc")
			if err != nil {
				t.Fatal(err)
			}
			if !enc.Encrypted || enc.EncryptionMode != "x25519" || !strings.HasSuffix(enc.StorageKey, ".archive.gz.age") ||
				enc.Trigger != models.TriggerManual || enc.SizeBytes != int64(len(f.oldBackupCT)) {
				t.Errorf("bkp_v1_enc = %+v", enc)
			}
			failed, err := s.GetBackupRecord(ctx, "bkp_v1_failed")
			if err != nil || failed.Status != models.StatusFailed || failed.ErrorMessage != "mongodump failed: exit status 1" {
				t.Errorf("bkp_v1_failed = %+v, %v", failed, err)
			}

			rst, err := s.GetRestoreRecord(ctx, "rst_v1")
			if err != nil || rst.BackupID != "bkp_v1_plain" || rst.TargetDatabase != "shop_rescue_20260925_130000" ||
				rst.Status != models.RestoreStatusCompleted || !rst.Verified {
				t.Errorf("rst_v1 = %+v, %v", rst, err)
			}

			ch, err := s.GetChannel(ctx, "ch_v1")
			if err != nil {
				t.Fatal(err)
			}
			if ch.Webhook == nil || ch.Webhook.URL != compatHookURL || ch.Webhook.Secret != compatHookSecret ||
				ch.Webhook.Headers["X-Token"] != "header-token-6" || !ch.Enabled {
				t.Errorf("ch_v1 = %+v", ch)
			}
			// Whatever the fixture held in plaintext is sealed now.
			var raw string
			if err = s.db.QueryRow(`SELECT group_concat(data) FROM (SELECT data FROM jobs UNION ALL SELECT data FROM notification_channels)`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"legacy-pass-1", compatHookSecret, "path-token-5", "header-token-6"} {
				if strings.Contains(raw, secret) {
					t.Errorf("%q is still stored in plaintext", secret)
				}
			}
			rule, err := s.GetRule(ctx, "rule_v1")
			if err != nil || len(rule.Events) != 2 || !slices.Equal(rule.ChannelIDs, []string{"ch_v1"}) || !slices.Equal(rule.JobIDs, []string{"job_v1"}) {
				t.Errorf("rule_v1 = %+v, %v", rule, err)
			}
		},
	},
	{
		version: 2,
		seed: func(t *testing.T, f *compatFixture) {
			// The release that introduced secretbox sealed the existing plaintext secrets
			// in place and recorded the key check value and the format marker.
			f.exec(t, `UPDATE jobs SET data = json_set(data, '$.mongo_uri', ?) WHERE id = 'job_v1'`,
				f.seal(t, tableJobs, "job_v1", fieldLegacyJobURI, compatLegacyJobURI))
			f.exec(t, `UPDATE notification_channels SET data = json_set(data, '$.webhook.url', ?, '$.webhook.secret', ?, '$.webhook.headers.X-Token', ?) WHERE id = 'ch_v1'`,
				f.seal(t, tableChannels, "ch_v1", "webhook.url", compatHookURL),
				f.seal(t, tableChannels, "ch_v1", "webhook.secret", compatHookSecret),
				f.seal(t, tableChannels, "ch_v1", "webhook.headers.X-Token", "header-token-6"))
			f.exec(t, `INSERT INTO settings (key, value) VALUES (?, ?), (?, ?)`,
				keyCheckSetting, f.seal(t, tableSettings, keyCheckSetting, fieldSettingValue, keyCheckPlaintext),
				secretsFormatSetting, secretsFormatCurrent)

			f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, last_login_at) VALUES (?, ?, ?, ?, ?, ?)`,
				"usr_v1", "Admin", compatPassword, ns(0), ns(time.Minute), ns(time.Hour))
			f.exec(t, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
				strings.Repeat("5e", 32), "usr_v1", "csrf-v1", ns(time.Hour), ns(2*time.Hour), ns(24*time.Hour))
			f.exec(t, `INSERT INTO api_keys (id, name, prefix, key_hash, created_by, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				"key_v1", "ci", "mrk_v1aaaa", strings.Repeat("a1", 32), "usr_v1", ns(0), nil)

			f.exec(t, `INSERT INTO connections (id, name, data) VALUES (?, ?, ?)`, "conn_v2", "production", jsonDoc(t, map[string]any{
				"id": "conn_v2", "name": "production", "description": "replica set",
				"uri":        f.seal(t, tableConnections, "conn_v2", fieldConnectionURI, compatConnURI),
				"created_at": rfc(0), "updated_at": rfc(0), "last_test_ok": true, "server_version": "7.0.12",
			}))
			f.exec(t, `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				"job_v2", "hourly crm", "crm", 0, ns(time.Hour), "conn_v2", jsonDoc(t, map[string]any{
					"id": "job_v2", "name": "hourly crm", "cron_expression": "0 * * * *", "database": "crm", "storage_type": "local",
					"retention_days": 1, "gzip": false, "enabled": false, "connection_id": "conn_v2",
					"exclude_collections": []string{"sessions"}, "created_at": rfc(time.Hour), "updated_at": rfc(time.Hour),
				}))
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v2", "job_v2", "crm", "completed", ns(4*time.Hour), "conn_v2", jsonDoc(t, map[string]any{
					"id": "bkp_v2", "job_id": "job_v2", "database": "crm", "status": "completed", "connection_id": "conn_v2",
					"connection_name": "production", "storage_type": "local", "storage_key": "crm/2026/09/bkp_v2.archive",
					"size_bytes": 99, "sha256": strings.Repeat("ef", 32), "started_at": rfc(4 * time.Hour),
				}))
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			user, err := s.GetUserByUsername(ctx, "admin")
			if err != nil || user.ID != "usr_v1" || user.PasswordHash != compatPassword || user.LastLoginAt == nil ||
				!user.LastLoginAt.Equal(compatT0.Add(time.Hour)) {
				t.Errorf("usr_v1 = %+v, %v", user, err)
			}
			sess, err := s.GetSession(ctx, strings.Repeat("5e", 32))
			if err != nil || sess.UserID != "usr_v1" || sess.CSRFToken != "csrf-v1" || !sess.ExpiresAt.Equal(compatT0.Add(24*time.Hour)) {
				t.Errorf("session = %+v, %v", sess, err)
			}
			key, err := s.GetAPIKeyByPrefix(ctx, "mrk_v1aaaa")
			// Keys created before scopes existed had full rights.
			if err != nil || key.ID != "key_v1" || key.Hash != strings.Repeat("a1", 32) || key.Scope != auth.ScopeAdmin ||
				key.CreatedBy != "usr_v1" || key.LastUsedAt != nil {
				t.Errorf("key_v1 = %+v, %v", key, err)
			}
			conn, err := s.GetConnection(ctx, "conn_v2")
			if err != nil || conn.URI != compatConnURI || conn.Name != "production" || conn.Description != "replica set" ||
				!conn.LastTestOK || conn.ServerVersion != "7.0.12" {
				t.Errorf("conn_v2 = %+v, %v", conn, err)
			}
			job, err := s.GetJob(ctx, "job_v2")
			if err != nil || job.ConnectionID != "conn_v2" || job.Enabled || !slices.Equal(job.ExcludeCollections, []string{"sessions"}) {
				t.Errorf("job_v2 = %+v, %v", job, err)
			}
			rec, err := s.GetBackupRecord(ctx, "bkp_v2")
			if err != nil || rec.ConnectionID != "conn_v2" || rec.ConnectionName != "production" || rec.Trigger != models.TriggerScheduled {
				t.Errorf("bkp_v2 = %+v, %v", rec, err)
			}
			if list, err := s.ListBackupRecords(ctx, "crm"); err != nil || len(list) != 1 {
				t.Errorf("ListBackupRecords(crm) = %d, %v", len(list), err)
			}
		},
	},
	{
		version: 3,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO storage_targets (id, name, is_default, data) VALUES (?, ?, ?, ?), (?, ?, ?, ?)`,
				"tgt_local", "Local disk", 1, jsonDoc(t, map[string]any{
					"id": "tgt_local", "name": "Local disk", "type": "local", "is_default": true,
					"local": map[string]any{"path": "/var/lib/mongorescue/backups"}, "created_at": rfc(0), "updated_at": rfc(0), "last_test_ok": false,
				}),
				"tgt_s3", "Offsite", 0, jsonDoc(t, map[string]any{
					"id": "tgt_s3", "name": "Offsite", "type": "s3", "is_default": false,
					"s3": map[string]any{"endpoint": "https://s3.example.com", "region": "eu-central-1", "bucket": "backups", "prefix": "prod/",
						"access_key_id": "AKIAEXAMPLE", "secret_access_key": f.seal(t, tableStorageTargets, "tgt_s3", fieldS3SecretKey, compatS3Secret),
						"use_path_style": true},
					"created_at": rfc(0), "updated_at": rfc(0), "last_test_ok": true,
				}))
			f.exec(t, `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, storage_target_id, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"job_v3", "offsite shop", "shop", 1, ns(5*time.Hour), "conn_v2", "tgt_s3", jsonDoc(t, map[string]any{
					"id": "job_v3", "name": "offsite shop", "cron_expression": "30 2 * * 0", "database": "shop", "storage_type": "s3",
					"storage_target_id": "tgt_s3", "retention_count": 4, "gzip": true, "enabled": true, "connection_id": "conn_v2",
					"created_at": rfc(5 * time.Hour), "updated_at": rfc(5 * time.Hour),
				}))
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v3", "job_v3", "shop", "completed", ns(6*time.Hour), "conn_v2", "tgt_s3", jsonDoc(t, map[string]any{
					"id": "bkp_v3", "job_id": "job_v3", "database": "shop", "status": "completed", "connection_id": "conn_v2",
					"storage_type": "s3", "storage_target_id": "tgt_s3", "storage_target_name": "Offsite",
					"storage_key": "shop/2026/09/bkp_v3.archive.gz.age", "encrypted": true, "encryption_mode": "x25519",
					"size_bytes": len(f.oldBackupCT), "started_at": rfc(6 * time.Hour),
				}))
			// Dashboard-managed settings, including a sealed age identity.
			identity, _ := json.Marshal(f.ageIdentity)
			f.exec(t, `INSERT INTO settings (key, value) VALUES (?, ?), (?, ?), (?, ?), (?, ?), (?, ?)`,
				settings.KeyDefaultRetentionDays, "14",
				settings.KeyEncryptionEnabled, "true",
				settings.KeyEncryptionMode, `"x25519"`,
				settings.KeyEncryptionRecipients, jsonDoc(t, []string{f.ageRecip}),
				settings.KeyEncryptionIdentity, f.seal(t, tableSettings, settings.KeyEncryptionIdentity, fieldSettingValue, string(identity)))
		},
		check: func(t *testing.T, f *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			s3, err := s.GetStorageTarget(ctx, "tgt_s3")
			if err != nil || s3.S3 == nil || s3.S3.SecretAccessKey != compatS3Secret || s3.S3.Bucket != "backups" ||
				s3.S3.Prefix != "prod/" || !s3.S3.UsePathStyle || s3.IsDefault {
				t.Errorf("tgt_s3 = %+v, %v", s3, err)
			}
			local, err := s.GetStorageTarget(ctx, "tgt_local")
			if err != nil || !local.IsDefault || local.Local == nil || local.Local.Path != "/var/lib/mongorescue/backups" {
				t.Errorf("tgt_local = %+v, %v", local, err)
			}
			job, err := s.GetJob(ctx, "job_v3")
			if err != nil || job.StorageTargetID != "tgt_s3" || job.RetentionCount != 4 {
				t.Errorf("job_v3 = %+v, %v", job, err)
			}
			rec, err := s.GetBackupRecord(ctx, "bkp_v3")
			if err != nil || rec.StorageTargetID != "tgt_s3" || rec.StorageTargetName != "Offsite" || !rec.Encrypted {
				t.Errorf("bkp_v3 = %+v, %v", rec, err)
			}

			// The settings still decode, and the stored identity still decrypts a backup
			// encrypted before the upgrade.
			svc, err := settings.NewService(ctx, s, settings.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatalf("settings from the upgraded database: %v", err)
			}
			cur := svc.Current()
			if cur.General.DefaultRetentionDays != 14 || !cur.Encryption.Enabled || cur.Encryption.Identity != f.ageIdentity ||
				!slices.Equal(cur.Encryption.Recipients, []string{f.ageRecip}) || svc.Encryptor() == nil {
				t.Errorf("settings = %+v", cur)
			}
			dec := svc.Decryptor()
			if dec == nil {
				t.Fatal("no decryptor from the upgraded settings")
			}
			r, err := dec.Decrypt(bytes.NewReader(f.oldBackupCT))
			if err != nil {
				t.Fatalf("decrypt a backup taken before the upgrade: %v", err)
			}
			if got, err := io.ReadAll(r); err != nil || string(got) != "archive taken before the upgrade" {
				t.Fatalf("decrypted %q, %v", got, err)
			}
		},
	},
	{
		version: 4,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO api_keys (id, name, prefix, key_hash, created_by, created_at, last_used_at, scope) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"key_v4", "grafana", "mrk_v4bbbb", strings.Repeat("b2", 32), "usr_v1", ns(7*time.Hour), ns(8*time.Hour), "read")
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			key, err := s.GetAPIKeyByPrefix(context.Background(), "mrk_v4bbbb")
			if err != nil || key.Scope != auth.ScopeRead || key.LastUsedAt == nil || !key.LastUsedAt.Equal(compatT0.Add(8*time.Hour)) {
				t.Errorf("key_v4 = %+v, %v", key, err)
			}
			if keys, err := s.ListAPIKeys(context.Background()); err != nil || len(keys) != 2 {
				t.Errorf("ListAPIKeys = %d, %v", len(keys), err)
			}
		},
	},
	{
		version: 5,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO audit_log (at, api_key_id, api_key_name, transport, tool, arguments, result, error, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ns(9*time.Hour), "key_v4", "grafana", "http", "list_backups", `{"database":"shop"}`, "ok", "", 12)
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			e := findAudit(t, s, "list_backups")
			if e.APIKeyID != "key_v4" || e.Transport != "http" || e.Result != "ok" || e.DurationMS != 12 || e.Count != 1 ||
				e.HTTPStatus != 0 || string(e.Arguments) != `{"database":"shop"}` {
				t.Errorf("audit v5 = %+v", e)
			}
		},
	},
	{
		version: 6,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO audit_log (at, api_key_id, api_key_name, transport, tool, arguments, result, error, duration_ms, count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ns(10*time.Hour), "key_v4", "grafana", "stdio", "start_backup", `{}`, "denied", "forbidden: operator scope required", 0, 3)
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			if e := findAudit(t, s, "start_backup"); e.Count != 3 || e.Result != "denied" || e.Error != "forbidden: operator scope required" {
				t.Errorf("audit v6 = %+v", e)
			}
		},
	},
	{
		version: 7,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO audit_log (at, api_key_id, api_key_name, transport, tool, arguments, result, error, duration_ms, count, http_status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ns(11*time.Hour), "key_v1", "ci", "rest", "GET /api/v1/backups", `{}`, "ok", "", 3, 1, 200)
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			if e := findAudit(t, s, "GET /api/v1/backups"); e.HTTPStatus != 200 || e.Transport != "rest" {
				t.Errorf("audit v7 = %+v", e)
			}
		},
	},
	{
		version: 8,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v8", "", "shop", "completed", ns(12*time.Hour), "conn_v2", "tgt_local", jsonDoc(t, map[string]any{
					"id": "bkp_v8", "trigger": "mcp", "database": "shop", "status": "completed", "connection_id": "conn_v2",
					"storage_type": "local", "storage_target_id": "tgt_local", "storage_key": "shop/2026/09/bkp_v8.archive.gz",
					"started_at": rfc(12 * time.Hour),
				}))
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			if rec, err := s.GetBackupRecord(context.Background(), "bkp_v8"); err != nil || rec.Trigger != models.TriggerMCP {
				t.Errorf("bkp_v8 = %+v, %v", rec, err)
			}
		},
	},
	{
		version: 9,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, retry_of, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v9", "job_v1", "shop", "completed", ns(13*time.Hour), "conn_v2", "tgt_local", "bkp_v1_failed", jsonDoc(t, map[string]any{
					"id": "bkp_v9", "job_id": "job_v1", "trigger": "manual", "database": "shop", "status": "completed",
					"connection_id": "conn_v2", "storage_type": "local", "storage_target_id": "tgt_local",
					"storage_key": "shop/2026/09/bkp_v9.archive.gz", "retry_of": "bkp_v1_failed", "started_at": rfc(13 * time.Hour),
				}))
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			rec, err := s.GetBackupRecord(context.Background(), "bkp_v9")
			if err != nil || rec.RetryOf != "bkp_v1_failed" || rec.Trigger != models.TriggerManual {
				t.Errorf("bkp_v9 = %+v, %v", rec, err)
			}
			if orig, err := s.GetBackupRecord(context.Background(), "bkp_v1_failed"); err != nil || orig.Status != models.StatusFailed {
				t.Errorf("the retried backup must stay as it was: %+v, %v", orig, err)
			}
			// Migration 0011 backfills the phases of older records from started_at.
			if rec != nil && (rec.Phases.Queued == nil || !rec.Phases.Queued.Equal(compatT0.Add(13*time.Hour))) {
				t.Errorf("bkp_v9 phases = %+v, want queued backfilled from started_at", rec.Phases)
			}
		},
	},
	{
		// Schema 0010 only adds indexes; the filtered queries must read every older row.
		version: 10,
		seed:    func(*testing.T, *compatFixture) {},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			page, err := s.QueryBackupRecords(ctx, BackupFilter{RetryOf: "bkp_v1_failed", Limit: 10})
			if err != nil || page.Total != 1 || page.Rows[0].Record.ID != "bkp_v9" {
				t.Errorf("retries of bkp_v1_failed = %+v, %v; want bkp_v9", page, err)
			}
			page, err = s.QueryBackupRecords(ctx, BackupFilter{Status: models.StatusFailed, Search: "v1_FAILED"})
			if err != nil || page.Total != 1 || page.Rows[0].RetriedBy == nil || page.Rows[0].RetriedBy.ID != "bkp_v9" {
				t.Errorf("bkp_v1_failed = %+v, %v; want it retried by bkp_v9", page, err)
			}
			all, err := s.ListBackupRecords(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if page, err := s.QueryBackupRecords(ctx, BackupFilter{Limit: 1}); err != nil || page.Total != len(all) {
				t.Errorf("total = %+v, %v; want %d", page, err, len(all))
			}
			// The size_bytes column is backfilled from the records.
			var want int64
			for _, b := range all {
				if b.Status == models.StatusCompleted {
					want += b.SizeBytes
				}
			}
			if st, err := s.BackupStats(ctx, compatT0); err != nil || st.CompletedBytes != want || want == 0 {
				t.Errorf("completed bytes = %+v, %v; want %d (> 0)", st, err, want)
			}
		},
	},
	{
		version: 11,
		seed: func(t *testing.T, f *compatFixture) {
			phases := map[string]any{"queued": rfc(14 * time.Hour), "started": rfc(14 * time.Hour), "finished": rfc(15 * time.Hour)}
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, retry_of, phases, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v11", "job_v1", "shop", "cancelled", ns(14*time.Hour), "conn_v2", "tgt_local", "", jsonDoc(t, phases), jsonDoc(t, map[string]any{
					"id": "bkp_v11", "job_id": "job_v1", "trigger": "scheduled", "database": "shop", "status": "cancelled",
					"connection_id": "conn_v2", "storage_type": "local", "storage_target_id": "tgt_local",
					"storage_key": "shop/2026/09/bkp_v11.archive.gz", "started_at": rfc(14 * time.Hour),
					"cancelled_by": "admin", "cancelled_at": rfc(15 * time.Hour), "phases": phases,
					"error_message": "backup cancelled by admin",
				}))
			f.exec(t, `INSERT INTO restores (id, backup_id, source_database, target_database, status, started_at, phases, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"rst_v11", "bkp_v9", "shop", "shop", "cancelled", ns(14*time.Hour), jsonDoc(t, phases), jsonDoc(t, map[string]any{
					"id": "rst_v11", "backup_id": "bkp_v9", "source_database": "shop", "target_database": "shop",
					"status": "cancelled", "started_at": rfc(14 * time.Hour), "in_place": true, "cancelled_by": "admin",
					"cancelled_at": rfc(15 * time.Hour), "phases": phases,
					"warning": "cancelled midway: the in-place target shop may be PARTIALLY RESTORED",
				}))
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			b, err := s.GetBackupRecord(context.Background(), "bkp_v11")
			if err != nil || b.Status != models.StatusCancelled || b.CancelledBy != "admin" || b.CancelledAt == nil ||
				b.Phases.Started == nil || b.Phases.Finished == nil {
				t.Errorf("bkp_v11 = %+v, %v", b, err)
			}
			r, err := s.GetRestoreRecord(context.Background(), "rst_v11")
			if err != nil || r.Status != models.RestoreStatusCancelled || !r.InPlace || r.Warning == "" || r.Phases.Finished == nil {
				t.Errorf("rst_v11 = %+v, %v", r, err)
			}
		},
	},
	{
		version: 12,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO backup_manifests (backup_id, captured_at, data) VALUES (?, ?, ?)`,
				"bkp_v9", ns(13*time.Hour), jsonDoc(t, map[string]any{
					"captured_at": rfc(13 * time.Hour),
					"collections": []any{map[string]any{"name": "orders", "documents_min": 3, "documents_max": 4,
						"indexes": []any{map[string]any{"name": "_id_", "keys": "_id:1"}}}},
				}))
			f.exec(t, `INSERT INTO restore_tests (id, job_id, backup_id, started_at, status, data) VALUES (?, ?, ?, ?, ?, ?)`,
				"rt_v12", "job_v1", "bkp_v9", ns(14*time.Hour), "ok", jsonDoc(t, map[string]any{
					"id": "rt_v12", "job_id": "job_v1", "backup_id": "bkp_v9", "trigger": "scheduled", "status": "ok",
					"started_at": rfc(14 * time.Hour), "duration_seconds": 2.5, "collections": 1, "documents": 4, "dropped": true,
				}))
			f.exec(t, `INSERT INTO retention_log (at, job_id, backup_id, data) VALUES (?, ?, ?, ?)`,
				ns(15*time.Hour), "job_v1", "bkp_v1_old", jsonDoc(t, map[string]any{
					"id": 1, "time": rfc(15 * time.Hour), "job_id": "job_v1", "backup_id": "bkp_v1_old", "database": "shop",
					"backup_started_at": rfc(0), "size_bytes": 10, "reason": "max_age",
				}))
			f.exec(t, `INSERT INTO integrity_state (key, value) VALUES (?, ?)`, "sweep", `{"verified":3}`)
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			if m, err := s.GetManifest(ctx, "bkp_v9"); err != nil || m.Collection("orders") == nil || m.Collection("orders").DocumentsMax != 4 {
				t.Errorf("manifest of bkp_v9 = %+v, %v", m, err)
			}
			if tests, err := s.ListRestoreTests(ctx, "job_v1", 10); err != nil || len(tests) != 1 || tests[0].Status != models.RestoreTestOK {
				t.Errorf("restore tests = %+v, %v", tests, err)
			}
			if log, err := s.ListRetentionLog(ctx, "job_v1", 10); err != nil || len(log) != 1 || log[0].Reason != models.RetentionMaxAge {
				t.Errorf("retention log = %+v, %v", log, err)
			}
			var sweep struct {
				Verified int `json:"verified"`
			}
			if ok, err := s.LoadIntegrityState(ctx, "sweep", &sweep); err != nil || !ok || sweep.Verified != 3 {
				t.Errorf("sweep state = %+v, %v, %v", sweep, ok, err)
			}
		},
	},
	{
		version: 13,
		seed: func(t *testing.T, f *compatFixture) {
			f.exec(t, `INSERT INTO jobs (id, name, database_name, enabled, created_at, connection_id, storage_target_id, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"job_v13", "all prod", "", 1, ns(16*time.Hour), "conn_v2", "tgt_local", jsonDoc(t, map[string]any{
					"id": "job_v13", "name": "all prod", "cron_expression": "@daily", "database": "",
					"database_selection": map[string]any{"mode": "pattern", "databases": []string{"billing"},
						"include": []string{"prod_*"}, "exclude": []string{"prod_tmp?"}, "auto_include_new": false},
					"known_databases": []string{"prod_a", "prod_b"}, "parallelism": 2,
					"connection_id": "conn_v2", "storage_target_id": "tgt_local", "storage_type": "local",
					"gzip": true, "enabled": true, "created_at": rfc(16 * time.Hour), "updated_at": rfc(16 * time.Hour),
				}))
			f.exec(t, `INSERT INTO backups (id, job_id, database_name, status, started_at, connection_id, storage_target_id, retry_of, size_bytes, phases, run_id, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				"bkp_v13", "job_v13", "prod_a", "completed", ns(17*time.Hour), "conn_v2", "tgt_local", "", 77, "{}", "run_v13", jsonDoc(t, map[string]any{
					"id": "bkp_v13", "job_id": "job_v13", "run_id": "run_v13", "trigger": "scheduled", "database": "prod_a",
					"status": "completed", "connection_id": "conn_v2", "storage_type": "local", "storage_target_id": "tgt_local",
					"storage_key": "prod_a/2026/09/bkp_v13.archive.gz", "size_bytes": 77, "started_at": rfc(17 * time.Hour),
				}))
			f.exec(t, `INSERT INTO job_runs (id, job_id, started_at, status, data) VALUES (?, ?, ?, ?, ?)`,
				"run_v13", "job_v13", ns(17*time.Hour), "partial", jsonDoc(t, map[string]any{
					"id": "run_v13", "job_id": "job_v13", "trigger": "scheduled", "status": "partial", "started_at": rfc(17 * time.Hour),
					"databases": []any{
						map[string]any{"database": "prod_a", "backup_id": "bkp_v13", "status": "completed"},
						map[string]any{"database": "billing", "status": "failed", "error": "database not found"},
					},
					"new_databases": []string{"prod_c"},
				}))
		},
		check: func(t *testing.T, _ *compatFixture, s *SQLiteStore) {
			ctx := context.Background()
			job, err := s.GetJob(ctx, "job_v13")
			if err != nil || !job.MultiDatabase() || job.DatabaseSelection.Mode != models.SelectionPattern ||
				!slices.Equal(job.DatabaseSelection.Include, []string{"prod_*"}) || !slices.Equal(job.KnownDatabases, []string{"prod_a", "prod_b"}) ||
				job.Parallelism != 2 {
				t.Errorf("job_v13 = %+v, %v", job, err)
			}
			page, err := s.QueryBackupRecords(ctx, BackupFilter{RunID: "run_v13"})
			if err != nil || page.Total != 1 || page.Rows[0].Record.ID != "bkp_v13" || page.Rows[0].Record.RunID != "run_v13" {
				t.Errorf("backups of run_v13 = %+v, %v", page, err)
			}
			runs, err := s.ListJobRuns(ctx, "job_v13", 10)
			if err != nil || len(runs) != 1 || runs[0].Status != models.JobRunPartial || len(runs[0].Databases) != 2 ||
				!slices.Equal(runs[0].NewDatabases, []string{"prod_c"}) {
				t.Errorf("runs of job_v13 = %+v, %v", runs, err)
			}
		},
	},
}

func findAudit(t *testing.T, s *SQLiteStore, tool string) *audit.Entry {
	t.Helper()
	entries, err := s.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Tool == tool {
			return e
		}
	}
	t.Fatalf("audit entry for %q missing (have %d entries)", tool, len(entries))
	return nil
}

// buildCompatFixture writes a database at schema version upTo into path.
func buildCompatFixture(t *testing.T, path string, f *compatFixture, upTo int) {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	f.exec(t, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`)
	for _, m := range migrations {
		if m.version > upTo {
			break
		}
		f.exec(t, m.sql)
		f.exec(t, "INSERT INTO schema_migrations VALUES (?, ?, ?)", m.version, m.name, rfc(time.Duration(m.version)*time.Hour))
		for _, step := range compatSteps {
			if step.version == m.version {
				step.seed(t, f)
			}
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeFromEverySchemaVersion opens a fixture of every earlier schema version
// with this build and checks that every row and field survives the upgrade.
func TestUpgradeFromEverySchemaVersion(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := migrations[len(migrations)-1].version
	if last := compatSteps[len(compatSteps)-1].version; last != latest {
		t.Fatalf("the newest migration is %04d but the compatibility fixtures stop at %04d: add a compatStep with the rows it introduces", latest, last)
	}

	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := encryption.NewX25519Encryptor([]string{recipient})
	var ct bytes.Buffer
	w, _ := enc.Encrypt(&ct)
	_, _ = io.WriteString(w, "archive taken before the upgrade")
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	for version := 1; version <= latest; version++ {
		t.Run(fmt.Sprintf("from_%04d", version), func(t *testing.T) {
			f := &compatFixture{box: box, ageIdentity: identity, ageRecip: recipient, oldBackupCT: ct.Bytes()}
			path := filepath.Join(t.TempDir(), "mongorescue.db")
			buildCompatFixture(t, path, f, version)

			s, err := OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), WithSecretBox(box))
			if err != nil {
				t.Fatalf("open a version %d database: %v", version, err)
			}
			t.Cleanup(func() { _ = s.Close() })

			var current, applied int
			if err := s.db.QueryRow("SELECT MAX(version), COUNT(*) FROM schema_migrations").Scan(&current, &applied); err != nil {
				t.Fatal(err)
			}
			if current != latest || applied != latest {
				t.Fatalf("schema at %d with %d migrations; want %d", current, applied, latest)
			}
			for _, step := range compatSteps {
				if step.version <= version {
					step.check(t, f, s)
				}
			}
			if err := s.verifySecretsSealed(context.Background()); err != nil {
				t.Fatalf("secrets after the upgrade: %v", err)
			}
		})
	}
}

// schemaSnapshot returns the schema and every row of every table in a stable form.
func schemaSnapshot(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var out strings.Builder
	var tables []string
	rows, err := db.Query("SELECT type, name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "%s %s %s\n", typ, name, ddl)
		if typ == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	_ = rows.Close()
	for _, table := range tables {
		data, err := db.Query("SELECT * FROM " + table + " ORDER BY 1") //nolint:gosec // G202: table names come from sqlite_master of the fixture.
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := data.Columns()
		for data.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := data.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "%s %v\n", table, vals)
		}
		_ = data.Close()
	}
	return out.String()
}

// TestNewerSchemaIsRefusedUntouched opens a database migrated by a future release:
// the open must fail with ErrSchemaTooNew and change nothing, not even the secret
// key check or the format marker.
func TestNewerSchemaIsRefusedUntouched(t *testing.T) {
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	path := filepath.Join(t.TempDir(), "mongorescue.db")
	f := &compatFixture{box: box, ageIdentity: "unused", ageRecip: "unused"}
	buildCompatFixture(t, path, f, 1)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := migrations[len(migrations)-1].version
	// The future release applied every known migration and then some.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[1:] {
		if _, err = db.Exec(m.sql); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO schema_migrations VALUES (?, ?, ?)", m.version, m.name, "x"); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"CREATE TABLE future_things (id TEXT PRIMARY KEY NOT NULL, data TEXT NOT NULL) STRICT",
		"INSERT INTO future_things VALUES ('f1', '{}')",
		"ALTER TABLE backups ADD COLUMN future_column TEXT NOT NULL DEFAULT 'x'",
		fmt.Sprintf("INSERT INTO schema_migrations VALUES (%d, '%04d_future', 'x')", latest+1, latest+1),
		"DELETE FROM settings WHERE key = 'secrets_format'",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, path)

	for _, b := range []*secretbox.Box{box, nil} {
		var opts []Option
		if b != nil {
			opts = append(opts, WithSecretBox(b))
		}
		_, err = OpenSQLite(context.Background(), path, slog.New(slog.DiscardHandler), opts...)
		if !errors.Is(err, ErrSchemaTooNew) {
			t.Fatalf("OpenSQLite = %v; want ErrSchemaTooNew", err)
		}
		want := fmt.Sprintf("database is at version %d, this binary knows up to %d", latest+1, latest)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must say %q", err, want)
		}
	}
	if after := schemaSnapshot(t, path); after != before {
		t.Fatalf("a refused open changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
