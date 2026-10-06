package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Rows of the settings table that a secret key rotation uses besides the settings.
const (
	// keyRotationSetting holds the marker of a rotation in progress (KeyRotation as
	// JSON). It names keys by fingerprint only, so it is not sealed.
	keyRotationSetting = "secret_key_rotation"
	// retiredMACKeysSetting holds, sealed, the keys that MACed API keys imported from
	// MONGORESCUE_API_KEY before a rotation (JSON list of base64 keys).
	retiredMACKeysSetting = "auth_retired_imported_key_macs"
)

// importedKeyHashPrefix marks the stored hash of an API key imported from
// MONGORESCUE_API_KEY and MACed with a subkey of secret.key (see internal/auth).
const importedKeyHashPrefix = "hmac-sha256:"

// maxRetiredMACKeys bounds the retired imported-key MAC keys kept across rotations.
const maxRetiredMACKeys = 16

// Sentinel errors of secret key rotations.
var (
	// ErrKeyRotationPending is returned by BeginKeyRotation while another rotation
	// is recorded as in progress.
	ErrKeyRotationPending = errors.New("store: a secret key rotation is already in progress")
	// ErrCommitUncertain is returned by RotateSecretBox when the commit failed and
	// the database could not be read to tell whether it took effect; the startup
	// recovery decides (keep secret.key.next).
	ErrCommitUncertain = errors.New("store: the secret key rotation may or may not have committed")
)

// KeyRotation is the marker of a secret key rotation in progress. It is written
// before the re-sealing transaction and removed once the new key file is installed,
// so a start after a crash can tell which key the database is sealed with.
type KeyRotation struct {
	// OldFingerprint and NewFingerprint identify the keys (secretbox.Fingerprint).
	OldFingerprint string `json:"old_fingerprint"`
	NewFingerprint string `json:"new_fingerprint"`
	// StartedAt is when the rotation began.
	StartedAt time.Time `json:"started_at"`
	// Actor and ApprovalID name who asked for it and the approval it ran for.
	Actor      string `json:"actor,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

// BeginKeyRotation records m as the rotation in progress. It returns
// ErrKeyRotationPending while a marker exists.
func (s *SQLiteStore) BeginKeyRotation(ctx context.Context, m KeyRotation) error {
	if m.OldFingerprint == "" || m.NewFingerprint == "" {
		return fmt.Errorf("%w: key rotation marker needs both fingerprints", ErrInvalidRecord)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode key rotation marker: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		keyRotationSetting, string(raw))
	if err != nil {
		return fmt.Errorf("store: record key rotation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyRotationPending
	}
	return nil
}

// PendingKeyRotation returns the marker of the rotation in progress, or nil.
func (s *SQLiteStore) PendingKeyRotation(ctx context.Context) (*KeyRotation, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", keyRotationSetting).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read key rotation marker: %w", err)
	}
	var m KeyRotation
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("%w: key rotation marker: %w", ErrCorruptRecord, err)
	}
	return &m, nil
}

// EndKeyRotation removes the marker of the rotation in progress (no error when
// there is none).
func (s *SQLiteStore) EndKeyRotation(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", keyRotationSetting); err != nil {
		return fmt.Errorf("store: clear key rotation marker: %w", err)
	}
	return nil
}

// SecretKeyRotation describes the re-sealing of RotateSecretBox.
type SecretKeyRotation struct {
	// Next is the Box of the new key. Required.
	Next *secretbox.Box
	// RetiredImportedKeyMAC is the key that MACed API keys imported from
	// MONGORESCUE_API_KEY under the old secret key. While such keys exist it is kept,
	// sealed under the new key, so they keep verifying until their next use re-hashes
	// them (see internal/auth).
	RetiredImportedKeyMAC []byte
	// RetiredInstallID is the metadata backup install ID of the old key, recorded
	// in the same transaction (see RetiredInstalls) so that its snapshots, sealed
	// with the old key, are pruned once the delete grace period has passed.
	RetiredInstallID string
	// BeforeCommit, when set, runs inside the transaction right before it commits;
	// an error rolls everything back. It exists for fault injection in tests.
	BeforeCommit func() error
	// AfterCommit, when set, runs after a successful commit and its error is
	// treated like an error of the commit itself (fault injection in tests).
	AfterCommit func() error
	// OnCommit, when set, runs once the rotation committed, while writers and
	// WithKeyLocked still wait: in-memory state derived from the key (the metadata
	// backup install ID) switches together with the store. It must not call the
	// store.
	OnCommit func()
}

// KeyRotationResult counts what RotateSecretBox changed.
type KeyRotationResult struct {
	// Resealed counts the values sealed under the new key (the key check included).
	Resealed int
	// SessionsRevoked counts the dashboard sessions that were removed.
	SessionsRevoked int
	// Skipped names the values ("table|id|field") that could not be opened and were
	// left as they were (see CorruptRecords); they need to be entered again.
	Skipped []string
}

// sealedJSONField is a secret stored at path in the JSON data column of table.
type sealedJSONField struct {
	table string
	path  string
	field string
}

// sealedJSONFields lists the secrets that live in JSON data columns, apart from
// notification channels (re-sealed through notify.Channel.TransformSecrets).
var sealedJSONFields = []sealedJSONField{
	{tableConnections, "$.uri", fieldConnectionURI},
	{tableConnections, "$.post_restore_sealed", fieldConnectionPostRestore},
	{tableJobs, "$.mongo_uri", fieldLegacyJobURI},
	{tableJobs, "$.heartbeat_url", fieldJobHeartbeatURL},
	{tableStorageTargets, "$.s3.secret_access_key", fieldS3SecretKey},
}

// RotateSecretBox re-seals every sealed value of the database (connection URIs,
// job heartbeat URLs, notification channel secrets, storage credentials, secret
// settings, the key check value and the retired imported-key MAC keys) under r.Next
// in one transaction, and removes every dashboard session, whose cookies were bound
// to the old key's era. The transaction runs with synchronous=FULL, so a power loss
// after it returns cannot bring back the old key. Writers of secrets wait while it
// runs, and the store uses r.Next once it committed. On error nothing changed.
func (s *SQLiteStore) RotateSecretBox(ctx context.Context, r SecretKeyRotation) (*KeyRotationResult, error) {
	if r.Next == nil {
		return nil, ErrNoSecretBox
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	old := s.box
	if old == nil {
		return nil, ErrNoSecretBox
	}
	res, err := s.reseal(ctx, r, old)
	if err != nil {
		return nil, err
	}
	s.box = r.Next
	// Still under the key lock: code that pairs reads with the key (metadata
	// snapshots, see WithKeyLocked) sees the new box and what OnCommit set together.
	if r.OnCommit != nil {
		r.OnCommit()
	}
	return res, nil
}

// WithKeyLocked runs fn while no secret key rotation can commit: what fn reads from
// the database is sealed with the key in use before and after it. fn must not call
// store methods that seal or open values (they take the same lock).
func (s *SQLiteStore) WithKeyLocked(fn func() error) error {
	defer s.lockKey()()
	return fn()
}

// reseal runs the re-sealing transaction of RotateSecretBox on a dedicated
// connection, released before it returns. Caller holds keyMu.
func (s *SQLiteStore) reseal(ctx context.Context, r SecretKeyRotation, old *secretbox.Box) (*KeyRotationResult, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: rotate secret key: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "PRAGMA synchronous = FULL"); err != nil {
		return nil, fmt.Errorf("store: rotate secret key: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA synchronous = NORMAL")
	}()
	res := &KeyRotationResult{}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: rotate secret key: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	rs := resealer{old: old, next: r.Next, skipped: &res.Skipped}
	if err = rs.resealAll(ctx, tx, res); err != nil {
		return nil, fmt.Errorf("store: rotate secret key: %w", err)
	}
	res.Resealed -= len(res.Skipped)
	for _, at := range res.Skipped {
		s.logger.Warn("secret key rotation: a stored secret could not be opened and was left as it was; enter it again",
			slog.String("location", at))
	}
	if err = rs.retireImportedKeyMAC(ctx, tx, r.RetiredImportedKeyMAC, res); err != nil {
		return nil, fmt.Errorf("store: rotate secret key: %w", err)
	}
	if r.RetiredInstallID != "" {
		list, listErr := readRetiredInstalls(ctx, tx)
		if listErr != nil {
			return nil, fmt.Errorf("store: rotate secret key: %w", listErr)
		}
		list = append(list, RetiredInstall{InstallID: r.RetiredInstallID, RetiredAt: time.Now().UTC()})
		if err = writeRetiredInstalls(ctx, tx, list); err != nil {
			return nil, fmt.Errorf("store: rotate secret key: %w", err)
		}
	}
	n, err := tx.ExecContext(ctx, "DELETE FROM sessions")
	if err != nil {
		return nil, fmt.Errorf("store: rotate secret key: revoke sessions: %w", err)
	}
	revoked, _ := n.RowsAffected()
	res.SessionsRevoked = int(revoked)
	if r.BeforeCommit != nil {
		if err = r.BeforeCommit(); err != nil {
			return nil, err
		}
	}
	err = tx.Commit()
	committed = true // a failed commit has ended the transaction too
	if err == nil && r.AfterCommit != nil {
		err = r.AfterCommit()
	}
	if err != nil {
		// A failed commit does not prove that nothing was written (an fsync error
		// after the WAL frame): the key check value tells which key the database is
		// sealed with now.
		switch rotated, checkErr := keyCheckOpens(ctx, conn, r.Next); {
		case checkErr != nil:
			return nil, fmt.Errorf("%w: commit: %w (check: %w)", ErrCommitUncertain, err, checkErr)
		case !rotated:
			return nil, fmt.Errorf("store: rotate secret key: commit: %w", err)
		}
		s.logger.Warn("secret key rotation: the commit reported an error but the database uses the new key", slog.Any("error", err))
	}
	return res, nil
}

// SealedWith reports whether box opens the stored key check value, that is whether
// the database is sealed with box's key.
func (s *SQLiteStore) SealedWith(ctx context.Context, box *secretbox.Box) (bool, error) {
	ok, err := keyCheckOpens(ctx, s.db, box)
	if err != nil {
		return false, fmt.Errorf("store: read key check value: %w", err)
	}
	return ok, nil
}

// keyCheckOpens reports whether box opens the stored key check value.
func keyCheckOpens(ctx context.Context, q queryer, box *secretbox.Box) (bool, error) {
	var kcv string
	if err := q.QueryRowContext(context.WithoutCancel(ctx), "SELECT value FROM settings WHERE key = ?", keyCheckSetting).Scan(&kcv); err != nil {
		return false, err
	}
	plain, err := box.Open(secretbox.At(tableSettings, keyCheckSetting, fieldSettingValue), kcv)
	return err == nil && plain == keyCheckPlaintext, nil
}

// resealer opens values with the old Box and seals them with the next one.
type resealer struct {
	old, next *secretbox.Box
	// skipped collects the locations of values that could not be opened.
	skipped *[]string
}

// reseal re-seals one value stored at at; empty values stay empty. A value that is
// not sealed in the current format or does not open (damaged, or planted) is left
// as it is and reported in skipped, like CorruptRecords: it was unreadable before
// and stays so, and it must not block the rotation. The key check value is the
// exception: without it the new key could not be verified.
func (r resealer) reseal(at secretbox.Binding, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	var plain string
	err := fmt.Errorf("%w: %s", ErrUnsealedSecret, at)
	if secretbox.IsSealed(v) {
		plain, err = r.old.Open(at, v)
	}
	if err != nil {
		if at.RecordID == keyCheckSetting && at.Table == tableSettings {
			return "", fmt.Errorf("decrypt %s: %w", at, err)
		}
		*r.skipped = append(*r.skipped, at.String())
		return v, nil
	}
	sealed, err := r.next.Seal(at, plain)
	if err != nil {
		return "", fmt.Errorf("encrypt %s: %w", at, err)
	}
	return sealed, nil
}

// resealAll re-seals the JSON fields, the notification channels and the sealed
// settings rows.
func (r resealer) resealAll(ctx context.Context, tx *sql.Tx, res *KeyRotationResult) error {
	for _, f := range sealedJSONFields {
		n, err := r.resealJSONField(ctx, tx, f)
		if err != nil {
			return err
		}
		res.Resealed += n
	}
	n, err := r.resealChannels(ctx, tx)
	if err != nil {
		return err
	}
	res.Resealed += n
	if n, err = r.resealApprovalSecrets(ctx, tx); err != nil {
		return err
	}
	res.Resealed += n
	keys := append(secretSettingKeys(), keyCheckSetting, retiredMACKeysSetting)
	n, err = r.resealSettings(ctx, tx, keys)
	if err != nil {
		return err
	}
	res.Resealed += n
	return nil
}

// idValue is a record ID and the value of one of its fields.
type idValue struct{ id, value string }

// collect runs query (two text columns) and returns its rows; the rows are closed
// before the caller writes on the same transaction.
func collect(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]idValue, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []idValue
	for rows.Next() {
		var v idValue
		if err := rows.Scan(&v.id, &v.value); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// resealJSONField re-seals f in every row of its table that holds a value.
func (r resealer) resealJSONField(ctx context.Context, tx *sql.Tx, f sealedJSONField) (int, error) {
	expr := "json_extract(data, '" + f.path + "')"
	list, err := collect(ctx, tx, "SELECT id, "+expr+" FROM "+f.table+" WHERE coalesce("+expr+", '') != ''") //nolint:gosec // G202: table and path are constants.
	if err != nil {
		return 0, fmt.Errorf("read %s.%s: %w", f.table, f.field, err)
	}
	for _, row := range list {
		v, err := r.reseal(secretbox.At(f.table, row.id, f.field), row.value)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE "+f.table+" SET data = json_set(data, '"+f.path+"', ?) WHERE id = ?", v, row.id); err != nil { //nolint:gosec // G202: table and path are constants.
			return 0, fmt.Errorf("write %s.%s: %w", f.table, f.field, err)
		}
	}
	return len(list), nil
}

// resealChannels re-seals every secret of every notification channel.
func (r resealer) resealChannels(ctx context.Context, tx *sql.Tx) (int, error) {
	chans, err := listRecordsStrict[notify.Channel](ctx, tx, "SELECT data FROM notification_channels")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ch := range chans {
		next, err := ch.TransformSecrets(func(field, v string) (string, error) {
			if v != "" {
				n++
			}
			return r.reseal(secretbox.At(tableChannels, ch.ID, field), v)
		})
		if err != nil {
			return 0, err
		}
		data, err := encode(next)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE notification_channels SET data = ? WHERE id = ?", data, ch.ID); err != nil {
			return 0, fmt.Errorf("write notification channel %s: %w", ch.ID, err)
		}
	}
	return n, nil
}

// resealSettings re-seals the settings rows named by keys that hold a value.
func (r resealer) resealSettings(ctx context.Context, tx *sql.Tx, keys []string) (int, error) {
	list, err := collect(ctx, tx, "SELECT key, value FROM settings WHERE key IN ('"+strings.Join(keys, "', '")+"') AND value != ''") //nolint:gosec // G202: setting keys are constants.
	if err != nil {
		return 0, fmt.Errorf("read settings: %w", err)
	}
	for _, row := range list {
		v, err := r.reseal(secretbox.At(tableSettings, row.id, fieldSettingValue), row.value)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE settings SET value = ? WHERE key = ?", v, row.id); err != nil {
			return 0, fmt.Errorf("write setting %s: %w", row.id, err)
		}
	}
	return len(list), nil
}

// resealApprovalSecrets re-seals the sealed secrets of approval requests: the
// post-restore commands a models.ApprovalPostRestoreCommands holds. Other approval
// secrets (password hashes) are not sealed and stay as they are.
func (r resealer) resealApprovalSecrets(ctx context.Context, tx *sql.Tx) (int, error) {
	list, err := collect(ctx, tx, "SELECT id, secret FROM approvals WHERE json_extract(data, '$.action') = ? AND secret != ''",
		string(models.ApprovalPostRestoreCommands))
	if err != nil {
		return 0, fmt.Errorf("read approval secrets: %w", err)
	}
	for _, row := range list {
		v, err := r.reseal(secretbox.At(tableApprovals, row.id, fieldApprovalSecret), row.value)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE approvals SET secret = ? WHERE id = ?", v, row.id); err != nil {
			return 0, fmt.Errorf("write approval secret %s: %w", row.id, err)
		}
	}
	return len(list), nil
}

// lockKey takes the read side of the key lock: a rotation (which takes the write
// side) never runs between sealing a value and writing it, or between reading a
// sealed value and opening it. Use it as defer s.lockKey()().
func (s *SQLiteStore) lockKey() func() {
	s.keyMu.RLock()
	return s.keyMu.RUnlock
}
