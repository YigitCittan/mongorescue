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

const userColumns = "id, username, password_hash, created_at, updated_at, last_login_at, role"

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

// insertUser inserts a user, mapping a username clash to auth.ErrUserExists. The
// role is always bound: an empty or unknown one is refused (auth.ErrInvalidRole),
// so the column default (admin, for users stored before roles existed) never
// applies to a new user.
func insertUser(ctx context.Context, e execer, u *auth.User) error {
	if !u.Role.Valid() {
		return fmt.Errorf("store: insert user: %w: %q", auth.ErrInvalidRole, u.Role)
	}
	_, err := e.ExecContext(ctx, "INSERT INTO users ("+userColumns+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.Username, u.PasswordHash, timeKey(u.CreatedAt), timeKey(u.UpdatedAt), nullTime(u.LastLoginAt), string(u.Role))
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

// ListUsers returns all users sorted by username. A row that cannot be read is
// skipped and reported through CorruptRecords.
func (s *SQLiteStore) ListUsers(ctx context.Context) ([]*auth.User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userColumns+" FROM users ORDER BY username, id")
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*auth.User, 0)
	for rows.Next() {
		u := new(auth.User)
		// Scan assigns columns in order, so the ID is known when a later column fails.
		if err := scanUserInto(rows, u); err != nil {
			if !errors.Is(err, ErrCorruptRecord) {
				return nil, err
			}
			s.reportCorrupt(tableUsers, u.ID, err)
			continue
		}
		s.clearCorrupt(tableUsers, u.ID)
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

// DeleteUser removes a user (sessions cascade). Neither the last user nor the last
// admin can be deleted.
func (s *SQLiteStore) DeleteUser(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
			return fmt.Errorf("store: count users: %w", err)
		}
		role, err := userRole(ctx, tx, id)
		if err != nil {
			return err
		}
		if n <= 1 {
			return auth.ErrLastUser
		}
		if role == auth.RoleAdmin {
			if err = refuseLastAdmin(ctx, tx); err != nil {
				return err
			}
		}
		// Sessions go with the user (ON DELETE CASCADE); delete explicitly as well so
		// revocation never depends on the foreign_keys pragma.
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return fmt.Errorf("store: revoke sessions: %w", err)
		}
		// API keys the user created would otherwise keep working with full rights.
		if _, err = tx.ExecContext(ctx, "DELETE FROM api_keys WHERE created_by = ?", id); err != nil {
			return fmt.Errorf("store: revoke api keys: %w", err)
		}
		return execOne(ctx, tx, auth.ErrUserNotFound, "DELETE FROM users WHERE id = ?", id)
	})
}

// UpdateUserRole sets the role of userID and revokes the user's sessions, in one
// transaction, refusing to demote the last admin and, when actorID is not "", to
// act for a user who is no longer an admin.
func (s *SQLiteStore) UpdateUserRole(ctx context.Context, actorID, userID string, role auth.Role, updatedAt time.Time) (auth.Role, error) {
	if !role.Valid() {
		return "", fmt.Errorf("store: update user role: %w: %q", auth.ErrInvalidRole, role)
	}
	var previous auth.Role
	txErr := s.withTx(ctx, func(tx *sql.Tx) error {
		if actorID != "" {
			if err := requireAdminRole(ctx, tx, actorID); err != nil {
				return err
			}
		}
		current, err := userRole(ctx, tx, userID)
		if err != nil {
			return err
		}
		previous = current
		if current == role {
			return nil
		}
		if current == auth.RoleAdmin {
			if err = refuseLastAdmin(ctx, tx); err != nil {
				return err
			}
		}
		if err = execOne(ctx, tx, auth.ErrUserNotFound, "UPDATE users SET role = ?, updated_at = ? WHERE id = ?",
			string(role), timeKey(updatedAt), userID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
			return fmt.Errorf("store: revoke sessions: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return previous, nil
}

// requireAdminRole refuses (with a *auth.ScopeError) when the user actorID no longer
// has the admin role, and with auth.ErrUnauthenticated when the user is gone.
func requireAdminRole(ctx context.Context, tx *sql.Tx, actorID string) error {
	role, err := userRole(ctx, tx, actorID)
	if errors.Is(err, auth.ErrUserNotFound) {
		return auth.ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if role != auth.RoleAdmin {
		return &auth.ScopeError{Have: role.Scope(), Need: auth.ScopeAdmin, Source: auth.SourceRole, Role: role}
	}
	return nil
}

// refuseLastAdmin returns auth.ErrLastAdmin unless more than one user has the admin
// role.
func refuseLastAdmin(ctx context.Context, tx *sql.Tx) error {
	admins, err := countAdmins(ctx, tx)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return auth.ErrLastAdmin
	}
	return nil
}

// userRole returns the stored role of id or auth.ErrUserNotFound.
func userRole(ctx context.Context, tx *sql.Tx, id string) (auth.Role, error) {
	var role string
	err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id = ?", id).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", auth.ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read user role: %w", err)
	}
	return auth.Role(role), nil
}

// countAdmins returns the number of users with the admin role.
func countAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE role = ?", string(auth.RoleAdmin)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count admins: %w", err)
	}
	return n, nil
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

// ListSessions returns the sessions of userID, or of every user when userID is "",
// most recently active first.
func (s *SQLiteStore) ListSessions(ctx context.Context, userID string) ([]*auth.Session, error) {
	query := `SELECT token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at FROM sessions`
	var args []any
	if userID != "" {
		query += " WHERE user_id = ?"
		args = append(args, userID)
	}
	query += " ORDER BY last_seen_at DESC, created_at DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*auth.Session
	for rows.Next() {
		var sess auth.Session
		var created, seen, expires int64
		if err := rows.Scan(&sess.TokenHash, &sess.UserID, &sess.CSRFToken, &created, &seen, &expires); err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromKey(created), fromKey(seen), fromKey(expires)
		out = append(out, &sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
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

// ListAPIKeys returns all API keys, newest first. A row that cannot be read is
// skipped and reported through CorruptRecords.
func (s *SQLiteStore) ListAPIKeys(ctx context.Context) ([]*auth.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+apiKeyColumns+" FROM api_keys ORDER BY created_at DESC, id")
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list := make([]*auth.APIKey, 0)
	for rows.Next() {
		k := new(auth.APIKey)
		if err := scanAPIKeyInto(rows, k); err != nil {
			if !errors.Is(err, ErrCorruptRecord) {
				return nil, err
			}
			s.reportCorrupt(tableAPIKeys, k.ID, err)
			continue
		}
		s.clearCorrupt(tableAPIKeys, k.ID)
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
	if err := scanUserInto(r, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// scanUserInto scans a users row into u. It returns sql.ErrNoRows unwrapped, and an
// error wrapping ErrCorruptRecord when a column does not convert.
func scanUserInto(r rowScanner, u *auth.User) error {
	var created, updated int64
	var lastLogin sql.NullInt64
	var role string
	if err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &updated, &lastLogin, &role); err != nil {
		return scanRowError("user", err)
	}
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt, u.Role = fromKey(created), fromKey(updated), nullableKey(lastLogin), auth.Role(role)
	return nil
}

func scanAPIKey(r rowScanner) (*auth.APIKey, error) {
	var k auth.APIKey
	if err := scanAPIKeyInto(r, &k); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return &k, nil
}

// scanAPIKeyInto scans an api_keys row into k, with the errors of scanUserInto.
func scanAPIKeyInto(r rowScanner, k *auth.APIKey) error {
	var created int64
	var lastUsed sql.NullInt64
	var scope string
	if err := r.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedBy, &created, &lastUsed, &scope); err != nil {
		return scanRowError("api key", err)
	}
	k.CreatedAt, k.LastUsedAt, k.Scope = fromKey(created), nullableKey(lastUsed), auth.Scope(scope)
	return nil
}

// scanRowError classifies a Scan error of a column-based row: sql.ErrNoRows is
// returned as is, a failed column conversion as ErrCorruptRecord, anything else
// (a failing connection) as a plain error.
func scanRowError(what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.HasPrefix(err.Error(), "sql: Scan error on column") {
		return unreadable(&columnScanError{err})
	}
	return fmt.Errorf("store: scan %s: %w", what, err)
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
