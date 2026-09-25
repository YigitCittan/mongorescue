package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// Compile-time check that SQLiteStore serves the auth port.
var _ auth.Repository = (*SQLiteStore)(nil)

const userColumns = "id, username, password_hash, created_at, updated_at, last_login_at"

// CountUsers returns the number of users.
func (s *SQLiteStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

// CreateFirstUser inserts u only while the users table is empty, in one transaction.
func (s *SQLiteStore) CreateFirstUser(ctx context.Context, u *auth.User) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
			return fmt.Errorf("store: count users: %w", err)
		}
		if n > 0 {
			return auth.ErrSetupCompleted
		}
		return insertUser(ctx, tx, u)
	})
}

// CreateUser inserts u or returns auth.ErrUserExists.
func (s *SQLiteStore) CreateUser(ctx context.Context, u *auth.User) error {
	return insertUser(ctx, s.db, u)
}

// insertUser inserts a user, mapping a username clash to auth.ErrUserExists.
func insertUser(ctx context.Context, e execer, u *auth.User) error {
	_, err := e.ExecContext(ctx, "INSERT INTO users ("+userColumns+") VALUES (?, ?, ?, ?, ?, ?)",
		u.ID, u.Username, u.PasswordHash, timeKey(u.CreatedAt), timeKey(u.UpdatedAt), nullTime(u.LastLoginAt))
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrUserExists
		}
		return fmt.Errorf("store: insert user: %w", err)
	}
	return nil
}

// GetUser returns a user or auth.ErrUserNotFound.
func (s *SQLiteStore) GetUser(ctx context.Context, id string) (*auth.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

// GetUserByUsername returns a user by case-insensitive username or auth.ErrUserNotFound.
func (s *SQLiteStore) GetUserByUsername(ctx context.Context, username string) (*auth.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE username = ?", username))
}

// ListUsers returns all users sorted by username.
func (s *SQLiteStore) ListUsers(ctx context.Context) ([]*auth.User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userColumns+" FROM users ORDER BY username, id")
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*auth.User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return list, nil
}

// UpdatePassword stores hash and revokes the user's sessions except keepSessionHash.
func (s *SQLiteStore) UpdatePassword(ctx context.Context, userID, hash string, updatedAt time.Time, keepSessionHash string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := execOne(ctx, tx, auth.ErrUserNotFound, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?",
			hash, timeKey(updatedAt), userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?", userID, keepSessionHash); err != nil {
			return fmt.Errorf("store: revoke sessions: %w", err)
		}
		return nil
	})
}

// RecordLogin sets the user's last login time.
func (s *SQLiteStore) RecordLogin(ctx context.Context, userID string, at time.Time) error {
	return execOne(ctx, s.db, auth.ErrUserNotFound, "UPDATE users SET last_login_at = ? WHERE id = ?", timeKey(at), userID)
}

// DeleteUser removes a user (sessions cascade). The last user cannot be deleted.
func (s *SQLiteStore) DeleteUser(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
			return fmt.Errorf("store: count users: %w", err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = ?", id).Scan(&exists); err != nil {
			return fmt.Errorf("store: check user: %w", err)
		}
		switch {
		case exists == 0:
			return auth.ErrUserNotFound
		case n <= 1:
			return auth.ErrLastUser
		}
		// Sessions go with the user (ON DELETE CASCADE); delete explicitly as well so
		// revocation never depends on the foreign_keys pragma.
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return fmt.Errorf("store: revoke sessions: %w", err)
		}
		// API keys the user created would otherwise keep working with full rights.
		if _, err := tx.ExecContext(ctx, "DELETE FROM api_keys WHERE created_by = ?", id); err != nil {
			return fmt.Errorf("store: revoke api keys: %w", err)
		}
		return execOne(ctx, tx, auth.ErrUserNotFound, "DELETE FROM users WHERE id = ?", id)
	})
}

// CreateSession stores a session.
func (s *SQLiteStore) CreateSession(ctx context.Context, sess *auth.Session) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, sess.TokenHash, sess.UserID, sess.CSRFToken,
		timeKey(sess.CreatedAt), timeKey(sess.LastSeenAt), timeKey(sess.ExpiresAt)); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSession returns a session by token hash or auth.ErrSessionNotFound.
func (s *SQLiteStore) GetSession(ctx context.Context, tokenHash string) (*auth.Session, error) {
	var sess auth.Session
	var created, seen, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at
		FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&sess.TokenHash, &sess.UserID, &sess.CSRFToken, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromKey(created), fromKey(seen), fromKey(expires)
	return &sess, nil
}

// TouchSession updates a session's last activity.
func (s *SQLiteStore) TouchSession(ctx context.Context, tokenHash string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?", timeKey(at), tokenHash); err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// DeleteSession removes a session; a missing one is not an error.
func (s *SQLiteStore) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions past their absolute expiry or idle too long.
func (s *SQLiteStore) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?",
		timeKey(now), timeKey(now.Add(-idle)))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	return int(n), nil
}

// apiKeyColumns lists column names only; key_hash holds SHA-256 digests.
const apiKeyColumns = "id, name, prefix, key_hash, created_by, created_at, last_used_at, scope" //nolint:gosec // G101: column names, not credentials.

// CreateAPIKey stores k. A key without a scope is stored with the least privileged
// one, auth.ScopeRead.
func (s *SQLiteStore) CreateAPIKey(ctx context.Context, k *auth.APIKey) error {
	scope := k.Scope
	if scope == "" {
		scope = auth.ScopeRead
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO api_keys ("+apiKeyColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		k.ID, k.Name, k.Prefix, k.Hash, k.CreatedBy, timeKey(k.CreatedAt), nullTime(k.LastUsedAt), string(scope)); err != nil {
		return fmt.Errorf("store: create api key: %w", err)
	}
	return nil
}

// ListAPIKeys returns all API keys, newest first.
func (s *SQLiteStore) ListAPIKeys(ctx context.Context) ([]*auth.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+apiKeyColumns+" FROM api_keys ORDER BY created_at DESC, id")
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*auth.APIKey, 0)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	return list, nil
}

// GetAPIKeyByPrefix returns the key with prefix or auth.ErrAPIKeyNotFound.
func (s *SQLiteStore) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*auth.APIKey, error) {
	return scanAPIKey(s.db.QueryRowContext(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE prefix = ?", prefix))
}

// TouchAPIKey records the last use of a key.
func (s *SQLiteStore) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE id = ?", timeKey(at), id); err != nil {
		return fmt.Errorf("store: touch api key: %w", err)
	}
	return nil
}

// DeleteAPIKey removes a key or returns auth.ErrAPIKeyNotFound.
func (s *SQLiteStore) DeleteAPIKey(ctx context.Context, id string) error {
	return execOne(ctx, s.db, auth.ErrAPIKeyNotFound, "DELETE FROM api_keys WHERE id = ?", id)
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(r rowScanner) (*auth.User, error) {
	var u auth.User
	var created, updated int64
	var lastLogin sql.NullInt64
	if err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &updated, &lastLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrUserNotFound
		}
		return nil, fmt.Errorf("store: scan user: %w", err)
	}
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromKey(created), fromKey(updated), nullableKey(lastLogin)
	return &u, nil
}

func scanAPIKey(r rowScanner) (*auth.APIKey, error) {
	var k auth.APIKey
	var created int64
	var lastUsed sql.NullInt64
	var scope string
	if err := r.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedBy, &created, &lastUsed, &scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("store: scan api key: %w", err)
	}
	k.CreatedAt, k.LastUsedAt, k.Scope = fromKey(created), nullableKey(lastUsed), auth.Scope(scope)
	return &k, nil
}

// fromKey converts a stored Unix-nanosecond timestamp back to UTC time.
func fromKey(n int64) time.Time {
	return time.Unix(0, n).UTC()
}

// nullableKey converts an optional stored timestamp.
func nullableKey(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromKey(n.Int64)
	return &t
}

// nullTime converts an optional time for storage.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeKey(*t)
}

// isUniqueViolation reports whether err is an SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
