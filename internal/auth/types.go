// Package auth implements MongoRescue's authentication: first-run setup, users with
// bcrypt passwords, server-side sessions with CSRF tokens, login throttling and API
// keys. It is the domain core; persistence is the Repository port implemented by
// internal/store and HTTP concerns (cookies, headers) live in internal/server.
package auth

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	// ErrInvalidCredentials is the single, generic login failure (unknown user or wrong
	// password), so responses never reveal which usernames exist.
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUnauthenticated is returned when a request carries no valid session or API key.
	ErrUnauthenticated = errors.New("auth: authentication required")
	// ErrSetupCompleted is returned by Setup once a user exists.
	ErrSetupCompleted = errors.New("auth: setup has already been completed")
	// ErrInvalidSetupCode is returned by Setup for a wrong setup code.
	ErrInvalidSetupCode = errors.New("auth: invalid setup code")
	// ErrThrottled is returned while a client is locked out after repeated failures;
	// it is wrapped in a *ThrottledError carrying the retry delay.
	ErrThrottled = errors.New("auth: too many failed attempts")
	// ErrInvalidPassword is returned when a new password violates the policy.
	ErrInvalidPassword = errors.New("auth: invalid password")
	// ErrInvalidUsername is returned for a malformed username.
	ErrInvalidUsername = errors.New("auth: invalid username")
	// ErrUserExists is returned when the username is taken (case-insensitively).
	ErrUserExists = errors.New("auth: username already exists")
	// ErrUserNotFound is returned for an unknown user ID.
	ErrUserNotFound = errors.New("auth: user not found")
	// ErrLastUser is returned when deleting the only remaining user.
	ErrLastUser = errors.New("auth: cannot delete the last user")
	// ErrDeleteSelf is returned when a user tries to delete their own account.
	ErrDeleteSelf = errors.New("auth: cannot delete your own user")
	// ErrCurrentPassword is returned when changing one's own password without the
	// correct current password.
	ErrCurrentPassword = errors.New("auth: current password is incorrect")
	// ErrAPIKeyNotFound is returned for an unknown API key ID.
	ErrAPIKeyNotFound = errors.New("auth: api key not found")
	// ErrInvalidName is returned for an empty or overlong API key name.
	ErrInvalidName = errors.New("auth: invalid name")
	// ErrCSRF is returned when a cookie-authenticated unsafe request lacks the CSRF token.
	ErrCSRF = errors.New("auth: missing or invalid CSRF token")
	// ErrSessionNotFound is returned by repositories for unknown session hashes.
	ErrSessionNotFound = errors.New("auth: session not found")
)

// ThrottledError reports a lockout and how long the client must wait.
type ThrottledError struct {
	// RetryAfter is the remaining lockout.
	RetryAfter time.Duration
}

// Error implements error.
func (e *ThrottledError) Error() string {
	return ErrThrottled.Error()
}

// Unwrap makes errors.Is(err, ErrThrottled) work.
func (e *ThrottledError) Unwrap() error { return ErrThrottled }

// User is an account. In v0.1.0 every user is an administrator.
type User struct {
	// ID is the unique identifier ("usr_" + hex).
	ID string `json:"id"`
	// Username is unique, compared case-insensitively.
	Username string `json:"username"`
	// PasswordHash is the bcrypt hash; it is never serialised.
	PasswordHash string `json:"-"`
	// CreatedAt is when the user was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the user (or their password) last changed; not serialised.
	UpdatedAt time.Time `json:"-"`
	// LastLoginAt is the time of the last successful login.
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Session is a server-side login session. Only the SHA-256 hash of the token is stored.
type Session struct {
	// TokenHash is the hex SHA-256 of the cookie token.
	TokenHash string
	// UserID is the owner.
	UserID string
	// CSRFToken must accompany unsafe requests made with this session.
	CSRFToken string
	// CreatedAt is when the session was created.
	CreatedAt time.Time
	// LastSeenAt is the last authenticated request (idle timeout reference).
	LastSeenAt time.Time
	// ExpiresAt is the absolute expiry.
	ExpiresAt time.Time
}

// APIKey is a long-lived credential for automation. Only the SHA-256 hash of the key
// is stored; the plaintext is returned once, on creation.
type APIKey struct {
	// ID is the unique identifier ("key_" + hex).
	ID string `json:"id"`
	// Name is a label chosen by the creator.
	Name string `json:"name"`
	// Prefix is the public, non-secret part of the key ("mr_<prefix>_...").
	Prefix string `json:"prefix"`
	// Hash is the hex SHA-256 of the full key; it is never serialised.
	Hash string `json:"-"`
	// CreatedBy is the ID of the user who created the key ("" for the static key).
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedAt is the creation time.
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is updated (at most once a minute) when the key authenticates.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Method identifies how a request was authenticated.
type Method string

// Authentication methods.
const (
	// MethodSession is a browser session cookie.
	MethodSession Method = "session"
	// MethodAPIKey is an API key (created in the dashboard or imported from the
	// deprecated MONGORESCUE_API_KEY).
	MethodAPIKey Method = "api_key"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	// User is the acting user; nil for the static environment API key.
	User *User
	// Method is how the request authenticated.
	Method Method
	// SessionHash identifies the session (MethodSession only).
	SessionHash string
	// CSRFToken is the session's CSRF token (MethodSession only).
	CSRFToken string
	// APIKeyID identifies the stored API key ("" for the static key).
	APIKeyID string
}

// UserID returns the acting user's ID or "".
func (p *Principal) UserID() string {
	if p == nil || p.User == nil {
		return ""
	}
	return p.User.ID
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by WithPrincipal, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// Repository is the persistence port for users, sessions and API keys.
// Implementations must be safe for concurrent use and return copies.
type Repository interface {
	// CountUsers returns the number of users.
	CountUsers(ctx context.Context) (int, error)
	// CreateFirstUser inserts u only if no user exists, atomically; otherwise it
	// returns ErrSetupCompleted.
	CreateFirstUser(ctx context.Context, u *User) error
	// CreateUser inserts u or returns ErrUserExists.
	CreateUser(ctx context.Context, u *User) error
	// GetUser returns a user or ErrUserNotFound.
	GetUser(ctx context.Context, id string) (*User, error)
	// GetUserByUsername looks a user up case-insensitively or returns ErrUserNotFound.
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	// ListUsers returns all users sorted by username.
	ListUsers(ctx context.Context) ([]*User, error)
	// UpdatePassword stores a new hash and deletes the user's sessions except
	// keepSessionHash (which may be ""), in one transaction.
	UpdatePassword(ctx context.Context, userID, hash string, updatedAt time.Time, keepSessionHash string) error
	// RecordLogin sets LastLoginAt.
	RecordLogin(ctx context.Context, userID string, at time.Time) error
	// DeleteUser removes a user, their sessions and the API keys they created, in one
	// transaction; it returns ErrUserNotFound, or ErrLastUser (atomically) when it is
	// the only user.
	DeleteUser(ctx context.Context, id string) error

	// CreateSession stores s.
	CreateSession(ctx context.Context, s *Session) error
	// GetSession returns a session by token hash or ErrSessionNotFound.
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	// TouchSession updates LastSeenAt.
	TouchSession(ctx context.Context, tokenHash string, at time.Time) error
	// DeleteSession removes one session (no error if absent).
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteExpiredSessions removes sessions past their absolute expiry or idle for
	// longer than idle, returning the number removed.
	DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int, error)

	// CreateAPIKey stores k.
	CreateAPIKey(ctx context.Context, k *APIKey) error
	// ListAPIKeys returns all keys, newest first.
	ListAPIKeys(ctx context.Context) ([]*APIKey, error)
	// GetAPIKeyByPrefix returns the key with prefix or ErrAPIKeyNotFound.
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error)
	// TouchAPIKey sets LastUsedAt.
	TouchAPIKey(ctx context.Context, id string, at time.Time) error
	// DeleteAPIKey removes a key or returns ErrAPIKeyNotFound.
	DeleteAPIKey(ctx context.Context, id string) error
}
