package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// ErrUnsealedSecret is returned when a secret field read from the database is not
// sealed in the current format (plaintext or a legacy value that escaped migration).
// Secrets are never returned in that case, since the value may have been planted.
var ErrUnsealedSecret = errors.New("store: stored secret is not encrypted")

// Key check value: a known plaintext sealed with the key when the database first sees
// one. Opening it proves that later starts use the same key.
const (
	keyCheckSetting   = "secret_key_check"
	keyCheckPlaintext = "mongorescue-secret-key-check-v1"
)

// Secrets format marker: recorded in settings by the one-time upgrade to the current
// sealed format. Once it is set, no plaintext or legacy value is ever accepted.
const (
	secretsFormatSetting = "secrets_format"
	secretsFormatCurrent = "sb2"
)

// secretsUpgraded reports whether the one-time upgrade to the current format ran.
func (s *SQLiteStore) secretsUpgraded(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", secretsFormatSetting).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read secrets format: %w", err)
	case v != secretsFormatCurrent:
		return false, fmt.Errorf("store: unknown secrets format %q (written by a newer release?)", v)
	}
	return true, nil
}

// unsealedSecretQueries select (id, field) of every non-empty secret field that is not
// sealed in the current format.
var unsealedSecretQueries = []struct {
	table string
	query string
}{
	{tableConnections, `SELECT id, 'uri' FROM connections WHERE ` + unsealedSQL(`json_extract(data, '$.uri')`)},
	{tableJobs, `SELECT id, 'mongo_uri' FROM jobs WHERE ` + unsealedSQL(`json_extract(data, '$.mongo_uri')`)},
	{tableChannels, `SELECT id, 'webhook.url' FROM notification_channels WHERE ` + unsealedSQL(`json_extract(data, '$.webhook.url')`)},
	{tableChannels, `SELECT id, 'webhook.secret' FROM notification_channels WHERE ` + unsealedSQL(`json_extract(data, '$.webhook.secret')`)},
	{tableChannels, `SELECT id, 'telegram.bot_token' FROM notification_channels WHERE ` + unsealedSQL(`json_extract(data, '$.telegram.bot_token')`)},
	{tableChannels, `SELECT id, 'email.password' FROM notification_channels WHERE ` + unsealedSQL(`json_extract(data, '$.email.password')`)},
	{tableChannels, `SELECT id, 'twilio.auth_token' FROM notification_channels WHERE ` + unsealedSQL(`json_extract(data, '$.twilio.auth_token')`)},
	{tableChannels, `SELECT c.id, 'webhook.headers.' || h.key FROM notification_channels c, json_each(c.data, '$.webhook.headers') h WHERE ` + unsealedSQL(`h.value`)},
	{tableStorageTargets, `SELECT id, 's3.secret_access_key' FROM storage_targets WHERE ` + unsealedSQL(`json_extract(data, '$.s3.secret_access_key')`)},
	{tableSettings, `SELECT key, 'value' FROM settings WHERE key IN ('` + strings.Join(append(secretSettingKeys(), keyCheckSetting), "', '") + `') AND ` + unsealedSQL(`value`)},
}

// unsealedSQL is an SQL condition that holds when expr is a non-empty value without
// the current sealed prefix.
func unsealedSQL(expr string) string {
	return "coalesce(" + expr + ", '') != '' AND substr(" + expr + ", 1, 4) != '" + secretbox.Prefix + "'"
}

// verifySecretsSealed fails with ErrUnsealedSecret, naming table|id|field, when any
// secret field holds plaintext or a legacy value.
func (s *SQLiteStore) verifySecretsSealed(ctx context.Context) error {
	for _, q := range unsealedSecretQueries {
		var id, field string
		err := s.db.QueryRowContext(ctx, q.query+" LIMIT 1").Scan(&id, &field)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("store: verify encrypted secrets: %w", err)
		}
		return fmt.Errorf("%w: %s (plaintext or a legacy value was written to the database after the one-time upgrade; restore the database from a backup or re-enter the secret)",
			ErrUnsealedSecret, secretbox.At(q.table, id, field))
	}
	return nil
}

// Secret locations: the table names and field names bound into every sealed value.
const (
	tableSettings      = "settings"
	tableConnections   = "connections"
	tableChannels      = "notification_channels"
	tableJobs          = "jobs"
	fieldConnectionURI = "uri"
	fieldLegacyJobURI  = "mongo_uri"
	fieldSettingValue  = "value"
)

// sealedSecretQueries count rows whose secret fields hold a sealed value (current or
// legacy format). Only the secret fields are inspected, never whole rows, so a name or
// description containing "sb1:" is not mistaken for a secret.
var sealedSecretQueries = []string{
	`SELECT COUNT(*) FROM connections WHERE ` + sealedSQL(`json_extract(data, '$.uri')`),
	`SELECT COUNT(*) FROM jobs WHERE ` + sealedSQL(`json_extract(data, '$.mongo_uri')`),
	`SELECT COUNT(*) FROM notification_channels WHERE ` + strings.Join([]string{
		sealedSQL(`json_extract(data, '$.webhook.url')`),
		sealedSQL(`json_extract(data, '$.webhook.secret')`),
		sealedSQL(`json_extract(data, '$.telegram.bot_token')`),
		sealedSQL(`json_extract(data, '$.email.password')`),
		sealedSQL(`json_extract(data, '$.twilio.auth_token')`),
		`EXISTS (SELECT 1 FROM json_each(data, '$.webhook.headers') WHERE ` + sealedSQL(`value`) + `)`,
	}, " OR "),
	`SELECT COUNT(*) FROM storage_targets WHERE ` + sealedSQL(`json_extract(data, '$.s3.secret_access_key')`),
	`SELECT COUNT(*) FROM settings WHERE key IN ('` + strings.Join(secretSettingKeys(), "', '") + `') AND ` + sealedSQL(`value`),
}

// secretSettingKeys returns the setting keys whose values are sealed.
func secretSettingKeys() []string {
	var out []string
	for _, k := range settings.Keys() {
		if settings.IsSecret(k) {
			out = append(out, k)
		}
	}
	return out
}

// sealedSQL is an SQL condition that holds when expr starts with a sealed-value prefix.
func sealedSQL(expr string) string {
	return "substr(coalesce(" + expr + ", ''), 1, 4) IN ('" + secretbox.Prefix + "', '" + secretbox.LegacyPrefix + "')"
}

// seal encrypts a secret for its location; empty values stay empty.
func (s *SQLiteStore) seal(at secretbox.Binding, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if s.box == nil {
		return "", ErrNoSecretBox
	}
	sealed, err := s.box.Seal(at, v)
	if err != nil {
		return "", fmt.Errorf("store: encrypt %s: %w", at, err)
	}
	return sealed, nil
}

// open decrypts a secret read from its location. Plaintext and legacy values are
// refused with ErrUnsealedSecret; the startup migration has converted them already.
func (s *SQLiteStore) open(at secretbox.Binding, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if s.box == nil {
		return "", ErrNoSecretBox
	}
	if !secretbox.IsSealed(v) {
		return "", fmt.Errorf("%w: %s", ErrUnsealedSecret, at)
	}
	plain, err := s.box.Open(at, v)
	if err != nil {
		return "", fmt.Errorf("store: decrypt %s: %w", at, err)
	}
	return plain, nil
}

// openMigrating decrypts a secret during the one-time format migration: current
// values are opened for their location, legacy "sb1:" values with the legacy format,
// and plaintext written by older releases is returned as is. current reports whether
// v was already in the current format.
func (s *SQLiteStore) openMigrating(at secretbox.Binding, v string) (plain string, current bool, err error) {
	switch {
	case v == "":
		return "", true, nil
	case secretbox.IsSealed(v):
		plain, err = s.box.Open(at, v)
		current = true
	case secretbox.IsLegacySealed(v):
		plain, err = s.box.OpenLegacy(v)
	default:
		plain = v
	}
	if err != nil {
		return "", false, fmt.Errorf("store: decrypt %s: %w", at, err)
	}
	return plain, current, nil
}

// checkSecretKey verifies the configured key against the stored key check value,
// creating it on first use. A database that already holds encrypted values but no key
// check value is refused as well, since its key cannot be verified. A legacy "sb1:"
// check value is verified and re-sealed in the current format, but only before the
// one-time upgrade (upgraded false).
func (s *SQLiteStore) checkSecretKey(ctx context.Context, upgraded bool) error {
	at := secretbox.At(tableSettings, keyCheckSetting, fieldSettingValue)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var kcv string
		err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", keyCheckSetting).Scan(&kcv)
		switch {
		case err == nil:
			if upgraded && !secretbox.IsSealed(kcv) {
				return fmt.Errorf("%w: %s", ErrUnsealedSecret, at)
			}
			plain, current, openErr := s.openMigrating(at, kcv)
			if openErr != nil || plain != keyCheckPlaintext || kcv == keyCheckPlaintext {
				return fmt.Errorf("%w (was secret.key lost or replaced, or MONGORESCUE_SECRET_KEY changed?)", secretbox.ErrSecretKeyMismatch)
			}
			if current {
				return nil
			}
			return s.putKeyCheck(ctx, tx, at)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: read key check value: %w", err)
		}

		for _, q := range sealedSecretQueries {
			var n int
			if err = tx.QueryRowContext(ctx, q).Scan(&n); err != nil {
				return fmt.Errorf("store: look for encrypted values: %w", err)
			}
			if n > 0 {
				return fmt.Errorf("%w: the database holds encrypted values but no key check value", secretbox.ErrSecretKeyMismatch)
			}
		}
		return s.putKeyCheck(ctx, tx, at)
	})
}

// putKeyCheck stores a fresh key check value.
func (s *SQLiteStore) putKeyCheck(ctx context.Context, tx *sql.Tx, at secretbox.Binding) error {
	sealed, err := s.seal(at, keyCheckPlaintext)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, keyCheckSetting, sealed); err != nil {
		return fmt.Errorf("store: store key check value: %w", err)
	}
	return nil
}

// upgradeSecrets re-seals, in one transaction, every stored secret that is not yet in
// the current format (plaintext written by earlier releases and legacy "sb1:" values,
// which were not bound to their location) and records the secrets format marker in
// the same transaction. It runs once per database: afterwards verifySecretsSealed
// refuses any value that is not sealed in the current format.
func (s *SQLiteStore) upgradeSecrets(ctx context.Context) error {
	var channels, conns, jobs int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		chans, err := listRecords[notify.Channel](ctx, tx, "SELECT data FROM notification_channels")
		if err != nil {
			return err
		}
		for _, ch := range chans {
			stale := false
			opened, openErr := ch.TransformSecrets(func(field, v string) (string, error) {
				plain, current, fieldErr := s.openMigrating(secretbox.At(tableChannels, ch.ID, field), v)
				stale = stale || !current
				return plain, fieldErr
			})
			if openErr != nil {
				return openErr
			}
			if !stale {
				continue
			}
			if err = s.putChannel(ctx, tx, opened); err != nil {
				return err
			}
			channels++
		}

		list, err := listRecords[models.Connection](ctx, tx, "SELECT data FROM connections")
		if err != nil {
			return err
		}
		for _, c := range list {
			plain, current, openErr := s.openMigrating(secretbox.At(tableConnections, c.ID, fieldConnectionURI), c.URI)
			if openErr != nil {
				return openErr
			}
			if current {
				continue
			}
			c.URI = plain
			if err = s.putConnection(ctx, tx, c); err != nil {
				return err
			}
			conns++
		}

		if _, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, secretsFormatSetting, secretsFormatCurrent); err != nil {
			return fmt.Errorf("store: record secrets format: %w", err)
		}

		legacy, err := listRecords[legacyJob](ctx, tx, "SELECT data FROM jobs WHERE coalesce(json_extract(data, '$.mongo_uri'), '') != ''")
		if err != nil {
			return err
		}
		for _, j := range legacy {
			plain, current, openErr := s.openMigrating(secretbox.At(tableJobs, j.ID, fieldLegacyJobURI), j.MongoURI)
			if openErr != nil {
				return openErr
			}
			if current {
				continue
			}
			j.MongoURI = plain
			if err = s.putLegacyJob(ctx, tx, j); err != nil {
				return err
			}
			jobs++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: upgrade secrets at rest: %w", err)
	}
	if channels > 0 || conns > 0 || jobs > 0 {
		s.logger.Info("re-encrypted credentials at rest in the current format",
			slog.Int("notification_channels", channels), slog.Int("connections", conns), slog.Int("legacy_jobs", jobs))
	}
	return nil
}
