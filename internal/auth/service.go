package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// Session lifetimes.
const (
	// DefaultIdleTimeout ends a session after this long without requests.
	DefaultIdleTimeout = 12 * time.Hour
	// DefaultAbsoluteTimeout ends a session this long after login regardless of use.
	DefaultAbsoluteTimeout = 7 * 24 * time.Hour
	// touchInterval bounds how often last-seen / last-used timestamps are written.
	touchInterval = time.Minute
)

// Service implements authentication use cases. It is safe for concurrent use.
type Service struct {
	repo       Repository
	logger     *slog.Logger
	now        func() time.Time
	bcryptCost int
	idle       time.Duration
	absolute   time.Duration
	// sessionPolicy, when set, supplies the session timeouts for every request.
	sessionPolicy func() (idle, absolute time.Duration)
	throttle      *Throttle

	// dummyHash is compared against for unknown usernames so that a login takes the
	// same time whether or not the account exists.
	dummyHash []byte
	// compare checks a password against a bcrypt hash (bcrypt.CompareHashAndPassword).
	compare func(hash, password []byte) error
	// hashSlots bounds concurrent bcrypt comparisons; requests wait up to hashWait for
	// a slot instead of being refused.
	hashSlots chan struct{}
	hashWait  time.Duration
	// importedKeyMACKey keys the hashes of keys imported from MONGORESCUE_API_KEY
	// (see WithImportedKeySecret).
	importedKeyMACKey []byte

	mu        sync.Mutex
	setupCode string
}

// Option customises a Service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithImportedKeySecret sets the key that MACs API keys imported from the deprecated
// MONGORESCUE_API_KEY: a subkey of secret.key (secretbox.DeriveSubkey with
// ImportedKeySubkeyPurpose), so imported keys keep verifying across restarts. Without
// it a random per-process key is used, and keys imported by that Service only verify
// until it stops.
func WithImportedKeySecret(key []byte) Option {
	return func(s *Service) { s.importedKeyMACKey = bytes.Clone(key) }
}

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithPasswordComparer replaces bcrypt.CompareHashAndPassword, so tests can observe
// how many password comparisons a request performs.
func WithPasswordComparer(fn func(hash, password []byte) error) Option {
	return func(s *Service) { s.compare = fn }
}

// WithCompareConcurrency bounds concurrent password comparisons to n (default
// 2 × GOMAXPROCS) and lets a request wait up to wait for a free slot (default 5s)
// before it is answered with a *ThrottledError.
func WithCompareConcurrency(n int, wait time.Duration) Option {
	return func(s *Service) {
		if n > 0 {
			s.hashSlots = make(chan struct{}, n)
		}
		if wait > 0 {
			s.hashWait = wait
		}
	}
}

// WithBcryptCost overrides DefaultBcryptCost (tests use bcrypt.MinCost).
func WithBcryptCost(cost int) Option { return func(s *Service) { s.bcryptCost = cost } }

// WithSessionTimeouts overrides the idle and absolute session lifetimes.
func WithSessionTimeouts(idle, absolute time.Duration) Option {
	return func(s *Service) { s.idle, s.absolute = idle, absolute }
}

// WithSessionPolicy reads the idle and absolute session lifetimes from fn on every
// request, so changed settings apply to existing sessions immediately. It overrides
// WithSessionTimeouts.
func WithSessionPolicy(fn func() (idle, absolute time.Duration)) Option {
	return func(s *Service) { s.sessionPolicy = fn }
}

// Now returns the current time on the service's clock (see WithClock), the clock
// session lifetimes and expiry times are measured on.
func (s *Service) Now() time.Time { return s.now() }

// sessionTimeouts returns the current idle and absolute session lifetimes.
func (s *Service) sessionTimeouts() (idle, absolute time.Duration) {
	if s.sessionPolicy != nil {
		return s.sessionPolicy()
	}
	return s.idle, s.absolute
}

// NewService returns a Service backed by repo.
func NewService(repo Repository, opts ...Option) (*Service, error) {
	s := &Service{
		repo:       repo,
		logger:     slog.Default(),
		now:        time.Now,
		bcryptCost: DefaultBcryptCost,
		idle:       DefaultIdleTimeout,
		absolute:   DefaultAbsoluteTimeout,
		compare:    bcrypt.CompareHashAndPassword,
		hashSlots:  make(chan struct{}, 2*runtime.GOMAXPROCS(0)),
		hashWait:   defaultHashWait,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.throttle = NewThrottle(s.now)
	dummy, err := randomBytes(18)
	if err != nil {
		return nil, err
	}
	if len(s.importedKeyMACKey) == 0 {
		if s.importedKeyMACKey, err = randomBytes(32); err != nil {
			return nil, err
		}
	}
	if s.dummyHash, err = bcrypt.GenerateFromPassword(dummy, s.bcryptCost); err != nil {
		return nil, fmt.Errorf("auth: prepare dummy hash: %w", err)
	}
	return s, nil
}

// Init prepares setup mode: when no user exists it generates a fresh one-time setup
// code (kept in memory only; a restart issues a new one). It reports whether setup is
// required; the caller shows SetupCode to the operator.
func (s *Service) Init(ctx context.Context) (bool, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil || !required {
		return required, err
	}
	code, err := newSetupCode()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.setupCode = code
	s.mu.Unlock()
	return true, nil
}

// SetupCode returns the current setup code, or "" outside setup mode.
func (s *Service) SetupCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupCode
}

// SetupRequired reports whether no user exists yet.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.repo.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// defaultHashWait bounds how long a login waits for a free comparison slot.
const defaultHashWait = 5 * time.Second

// comparePassword runs one bcrypt comparison once a global slot is free, waiting at
// most hashWait (or until ctx ends). Waiting instead of refusing keeps logins working
// for clients that share an address with a noisy one.
func (s *Service) comparePassword(ctx context.Context, hash, password []byte) (matched bool, err error) {
	timer := time.NewTimer(s.hashWait)
	defer timer.Stop()
	select {
	case s.hashSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, &ThrottledError{RetryAfter: busyRetry}
	}
	defer func() { <-s.hashSlots }()
	return s.compare(hash, password) == nil, nil
}

// LoginResult is a new session.
type LoginResult struct {
	// User is the logged-in user.
	User *User
	// Token is the plaintext session token for the cookie; it is never stored.
	Token string
	// CSRFToken must be sent as X-CSRF-Token on unsafe requests.
	CSRFToken string
	// ExpiresAt is the absolute session expiry.
	ExpiresAt time.Time
}

// Setup creates the first user from the one-time setup code and logs them in. It
// returns ErrSetupCompleted once a user exists and ErrInvalidSetupCode for a wrong
// code, or a *ThrottledError after more than SetupFreeAttempts wrong codes from
// clientIP within SetupWindow. A correct code is never refused because of earlier
// wrong ones: the per-IP budget only affects wrong codes.
func (s *Service) Setup(ctx context.Context, clientIP, code, username, password string) (*LoginResult, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	if !required {
		return nil, ErrSetupCompleted
	}

	s.mu.Lock()
	expected := s.setupCode
	s.mu.Unlock()
	given := normalizeSetupCode(code)
	want := normalizeSetupCode(expected)
	if expected == "" || subtle.ConstantTimeCompare([]byte(given), []byte(want)) != 1 {
		s.logger.Warn("setup attempt with an invalid setup code", logsafe.Attr("client_ip", clientIP))
		if wait := s.throttle.CountFailure("setup|"+clientIP, SetupFreeAttempts, SetupWindow); wait > 0 {
			return nil, &ThrottledError{RetryAfter: wait}
		}
		return nil, ErrInvalidSetupCode
	}

	user, err := s.newUser(username, password)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateFirstUser(ctx, user); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.setupCode == expected {
		s.setupCode = "" // single use
	}
	s.mu.Unlock()
	s.logger.Info("setup completed: first user created", slog.String("user_id", user.ID), logsafe.Attr("username", user.Username))
	return s.startSession(ctx, user)
}

// Login verifies credentials and starts a session. All failures return
// ErrInvalidCredentials, or a *ThrottledError while the (IP, username) pair is locked
// out or another attempt for it is in progress.
//
// The attempt is reserved before the password is checked, so parallel requests cannot
// exceed the failure budget. Every request that reaches the check performs exactly one
// bcrypt comparison, against the user's hash or a dummy hash for unknown users, and
// over-long passwords are rejected before the user is looked up, so timing does not
// reveal which usernames exist.
func (s *Service) Login(ctx context.Context, clientIP, username, password string) (*LoginResult, error) {
	attempt, wait := s.throttle.Reserve("login|"+clientIP+"|"+strings.ToLower(username), "ip|"+clientIP)
	if attempt == nil {
		return nil, &ThrottledError{RetryAfter: wait}
	}
	if len(password) > MaxPasswordBytes {
		attempt.Failed()
		return nil, ErrInvalidCredentials
	}

	user, err := s.repo.GetUserByUsername(ctx, username)
	hash := s.dummyHash
	switch {
	case err == nil:
		hash = []byte(user.PasswordHash)
	case errors.Is(err, ErrUserNotFound):
		user = nil
	default:
		attempt.Released()
		return nil, err
	}
	matched, err := s.comparePassword(ctx, hash, []byte(password))
	if err != nil {
		attempt.Released()
		return nil, err
	}
	if !matched || user == nil {
		attempt.Failed()
		s.logger.Warn("failed login", logsafe.Attr("client_ip", clientIP))
		return nil, ErrInvalidCredentials
	}
	attempt.Succeeded()

	now := s.now().UTC()
	if err := s.repo.RecordLogin(ctx, user.ID, now); err != nil {
		return nil, err
	}
	user.LastLoginAt = &now
	idle, _ := s.sessionTimeouts()
	if n, err := s.repo.DeleteExpiredSessions(ctx, now, idle); err != nil {
		s.logger.Warn("failed to purge expired sessions", slog.Any("error", err))
	} else if n > 0 {
		s.logger.Debug("purged expired sessions", slog.Int("count", n))
	}
	return s.startSession(ctx, user)
}

// startSession creates and stores a session for user.
func (s *Service) startSession(ctx context.Context, user *User) (*LoginResult, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	csrf, err := newToken()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	_, absolute := s.sessionTimeouts()
	sess := &Session{
		TokenHash:  HashToken(token),
		UserID:     user.ID,
		CSRFToken:  csrf,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(absolute),
	}
	if err := s.repo.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	return &LoginResult{User: user, Token: token, CSRFToken: csrf, ExpiresAt: sess.ExpiresAt}, nil
}

// Logout revokes the session identified by its plaintext token.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.DeleteSession(ctx, HashToken(token))
}

// AuthenticateSession resolves a session token to a Principal, enforcing the idle and
// absolute timeouts. Expired sessions are deleted.
func (s *Service) AuthenticateSession(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	hash := HashToken(token)
	sess, err := s.repo.GetSession(ctx, hash)
	if errors.Is(err, ErrSessionNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if s.sessionExpired(sess, now) {
		if err = s.repo.DeleteSession(ctx, hash); err != nil {
			s.logger.Warn("failed to delete expired session", slog.Any("error", err))
		}
		return nil, ErrUnauthenticated
	}
	user, err := s.repo.GetUser(ctx, sess.UserID)
	if errors.Is(err, ErrUserNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		if err := s.repo.TouchSession(ctx, hash, now); err != nil {
			s.logger.Warn("failed to update session activity", slog.Any("error", err))
		}
	}
	return &Principal{User: user, Method: MethodSession, SessionHash: hash, CSRFToken: sess.CSRFToken, Scope: ScopeAdmin}, nil
}

// AuthenticateAPIKey resolves an API key (a key created in the dashboard, or one
// imported from the deprecated MONGORESCUE_API_KEY) to a Principal.
func (s *Service) AuthenticateAPIKey(ctx context.Context, key string) (*Principal, error) {
	if key == "" || len(key) > maxAPIKeyLength {
		return nil, ErrUnauthenticated
	}
	stored, err := s.lookupAPIKey(ctx, key)
	if errors.Is(err, ErrAPIKeyNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if !s.apiKeyMatches(key, stored.Hash) {
		return nil, ErrUnauthenticated
	}
	now := s.now().UTC()
	if stored.LastUsedAt == nil || now.Sub(*stored.LastUsedAt) >= touchInterval {
		if err := s.repo.TouchAPIKey(ctx, stored.ID, now); err != nil {
			s.logger.Warn("failed to update api key last use", slog.String("api_key_id", stored.ID), slog.Any("error", err))
		}
	}
	scope := stored.Scope
	if !scope.Valid() {
		// Fail closed: a key with a scope this build does not know may only read.
		scope = ScopeRead
	}
	p := &Principal{Method: MethodAPIKey, APIKeyID: stored.ID, APIKeyName: stored.Name, Scope: scope}
	if stored.CreatedBy != "" {
		u, err := s.repo.GetUser(ctx, stored.CreatedBy)
		switch {
		case errors.Is(err, ErrUserNotFound):
			// Keys are revoked together with their creator; a leftover key of a
			// deleted user is refused rather than granted anonymous full rights.
			return nil, ErrUnauthenticated
		case err != nil:
			return nil, err
		}
		p.User = u
	}
	return p, nil
}

// CheckCSRF enforces the CSRF token on cookie-authenticated requests with unsafe
// methods. API-key requests are exempt: browsers never attach them automatically.
func CheckCSRF(p *Principal, method, token string) error {
	if p == nil || p.Method != MethodSession {
		return nil
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if token == "" || p.CSRFToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(p.CSRFToken)) != 1 {
		return ErrCSRF
	}
	return nil
}

// ListUsers returns all users.
func (s *Service) ListUsers(ctx context.Context) ([]*User, error) {
	return s.repo.ListUsers(ctx)
}

// CreateUser adds a user (every user is an administrator in v0.1.0).
func (s *Service) CreateUser(ctx context.Context, actor *Principal, username, password string) (*User, error) {
	user, err := s.newUser(username, password)
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateUser(ctx, user); err != nil {
		return nil, err
	}
	s.logger.Info("user created", slog.String("user_id", user.ID), logsafe.Attr("username", user.Username),
		slog.String("by", actor.UserID()))
	return user, nil
}

// DeleteUser removes a user and revokes their sessions and the API keys they created.
// Users cannot delete themselves, and the last user cannot be deleted.
func (s *Service) DeleteUser(ctx context.Context, actor *Principal, id string) error {
	if actor.UserID() == id {
		return ErrDeleteSelf
	}
	if err := s.repo.DeleteUser(ctx, id); err != nil {
		return err
	}
	s.logger.Info("user deleted", slog.String("user_id", id), slog.String("by", actor.UserID()))
	return nil
}

// ChangePassword sets a new password for userID. Changing one's own password needs
// the current one. Every other session of the user is revoked; the actor's own
// session survives a change of their own password.
func (s *Service) ChangePassword(ctx context.Context, actor *Principal, userID, current, newPassword string) error {
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	self := actor.UserID() == userID
	if self {
		attempt, wait := s.throttle.Reserve("password|"+userID, "")
		if attempt == nil {
			return &ThrottledError{RetryAfter: wait}
		}
		if len(current) > MaxPasswordBytes {
			attempt.Failed()
			return ErrCurrentPassword
		}
		matched, cmpErr := s.comparePassword(ctx, []byte(user.PasswordHash), []byte(current))
		if cmpErr != nil {
			attempt.Released()
			return cmpErr
		}
		if !matched {
			attempt.Failed()
			return ErrCurrentPassword
		}
		attempt.Succeeded()
	}
	if err = ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.bcryptCost)
	if err != nil {
		return fmt.Errorf("auth: hash password: %w", err)
	}
	keep := ""
	if self && actor.Method == MethodSession {
		keep = actor.SessionHash
	}
	if err := s.repo.UpdatePassword(ctx, userID, string(hash), s.now().UTC(), keep); err != nil {
		return err
	}
	s.logger.Info("password changed", slog.String("user_id", userID), slog.String("by", actor.UserID()))
	return nil
}

// ConfirmPassword re-authenticates actor for a sensitive action (such as
// downloading the recovery kit): actor must be a signed-in admin user (a browser
// session, never an API key) and password must be their current password. Wrong
// passwords count against the same per-user budget as password changes. It returns
// ErrUnauthenticated without an actor, ErrSessionRequired for API keys and the
// system, a *ScopeError without admin, ErrCurrentPassword for a wrong password and
// a *ThrottledError after too many wrong ones.
func (s *Service) ConfirmPassword(ctx context.Context, actor *Principal, password string) error {
	switch {
	case actor == nil:
		return ErrUnauthenticated
	case actor.Method != MethodSession || actor.User == nil:
		return ErrSessionRequired
	}
	if err := actor.Require(ScopeAdmin); err != nil {
		return err
	}
	user, err := s.repo.GetUser(ctx, actor.User.ID)
	if err != nil {
		return err
	}
	attempt, wait := s.throttle.Reserve("password|"+user.ID, "")
	if attempt == nil {
		return &ThrottledError{RetryAfter: wait}
	}
	if password == "" || len(password) > MaxPasswordBytes {
		attempt.Failed()
		return ErrCurrentPassword
	}
	matched, err := s.comparePassword(ctx, []byte(user.PasswordHash), []byte(password))
	if err != nil {
		attempt.Released()
		return err
	}
	if !matched {
		attempt.Failed()
		return ErrCurrentPassword
	}
	attempt.Succeeded()
	return nil
}

// ListAPIKeys returns all API keys (never their secrets).
func (s *Service) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	return s.repo.ListAPIKeys(ctx)
}

// CreateAPIKey issues a key named name with the given scope (ScopeRead when empty).
// The plaintext is returned only here. It returns ErrInvalidName for a bad name and
// ErrInvalidScope for an unknown scope.
func (s *Service) CreateAPIKey(ctx context.Context, actor *Principal, name string, scope Scope) (*APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxKeyNameLength || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, "", fmt.Errorf("%w: 1-%d printable characters", ErrInvalidName, maxKeyNameLength)
	}
	scope, err := ParseScope(string(scope))
	if err != nil {
		return nil, "", err
	}
	plain, prefix, err := newGeneratedKey()
	if err != nil {
		return nil, "", err
	}
	id, err := newID("key_")
	if err != nil {
		return nil, "", err
	}
	k := &APIKey{ID: id, Name: name, Prefix: prefix, Scope: scope, Hash: HashToken(plain), CreatedBy: actor.UserID(), CreatedAt: s.now().UTC()}
	if err := s.repo.CreateAPIKey(ctx, k); err != nil {
		return nil, "", err
	}
	s.logger.Info("api key created", slog.String("api_key_id", k.ID), slog.String("prefix", k.Prefix),
		logsafe.Attr("scope", string(k.Scope)), logsafe.Attr("by", actor.UserID()))
	return k, plain, nil
}

// Limits for keys imported from the deprecated MONGORESCUE_API_KEY.
const (
	minImportedKeyLength = 16
	maxAPIKeyLength      = 512
	// importedKeyName names an imported key in the dashboard.
	importedKeyName = "Imported from MONGORESCUE_API_KEY"
)

// importedKeyPrefix derives the lookup prefix of an imported key from its MAC. The
// leading upper-case "K" never occurs in generated (lower-case base32) prefixes.
func (s *Service) importedKeyPrefix(key string) string {
	return "K" + importedKeyMAC(s.importedKeyMACKey, key)[:apiKeyPrefixLen-1]
}

// legacyImportedKeyPrefix is the lookup prefix of a key imported by a release before
// imported keys were MACed: "L" and the start of its SHA-256 (stored as Hash). Such
// records keep working; only the presented key, never the import input, is hashed
// this way.
func legacyImportedKeyPrefix(presented string) string {
	return "L" + HashToken(presented)[:apiKeyPrefixLen-1]
}

// lookupAPIKey finds the stored record of a presented key: by the prefix of a
// generated key, otherwise by the prefix of an imported one (current MAC form first,
// then the SHA-256 form of earlier releases).
func (s *Service) lookupAPIKey(ctx context.Context, presented string) (*APIKey, error) {
	if prefix, ok := parseAPIKey(presented); ok {
		return s.repo.GetAPIKeyByPrefix(ctx, prefix)
	}
	if len(presented) < minImportedKeyLength {
		return nil, ErrAPIKeyNotFound
	}
	stored, err := s.repo.GetAPIKeyByPrefix(ctx, s.importedKeyPrefix(presented))
	if errors.Is(err, ErrAPIKeyNotFound) {
		return s.repo.GetAPIKeyByPrefix(ctx, legacyImportedKeyPrefix(presented))
	}
	return stored, err
}

// legacyImportedRecord returns a key record an earlier release imported in the
// SHA-256 form (lookup prefix "L", which generated lower-case prefixes never use), or
// nil when there is none. It does not hash the import input: the record is
// recognised by its form alone.
func (s *Service) legacyImportedRecord(ctx context.Context) (*APIKey, error) {
	keys, err := s.repo.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if len(k.Prefix) == apiKeyPrefixLen && k.Prefix[0] == 'L' && k.CreatedBy == "" &&
			!strings.HasPrefix(k.Hash, importedKeyMACScheme) {
			return k, nil
		}
	}
	return nil, nil
}

// apiKeyMatches compares a presented key with a stored hash in constant time: an
// HMAC for keys imported by this release, a SHA-256 digest otherwise.
func (s *Service) apiKeyMatches(presented, storedHash string) bool {
	if mac, ok := strings.CutPrefix(storedHash, importedKeyMACScheme); ok {
		return equalHashes(importedKeyMAC(s.importedKeyMACKey, presented), mac)
	}
	return equalHashes(HashToken(presented), storedHash)
}

// ImportAPIKey stores key, taken from the deprecated MONGORESCUE_API_KEY environment
// variable, as an API key (hash only), so existing automation keeps working after the
// variable is removed. It reports whether a key was created; importing the same key
// again is a no-op. The key can be revoked in the dashboard like any other.
func (s *Service) ImportAPIKey(ctx context.Context, key string) (bool, error) {
	if len(key) < minImportedKeyLength || len(key) > maxAPIKeyLength {
		return false, fmt.Errorf("%w: MONGORESCUE_API_KEY must be %d-%d characters", ErrInvalidName, minImportedKeyLength, maxAPIKeyLength)
	}
	prefix := s.importedKeyPrefix(key)
	if p, ok := parseAPIKey(key); ok {
		prefix = p
	}
	if _, err := s.repo.GetAPIKeyByPrefix(ctx, prefix); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrAPIKeyNotFound) {
		return false, err
	}
	// An earlier release may have imported the key (SHA-256 form, "L" prefix) and
	// stopped before recording the import as done. A second record would leave the
	// key working through the legacy lookup after the visible one is revoked, so
	// while such a record exists it stays the only imported key. Revoking it in the
	// dashboard lets the next start import the variable's current value.
	if legacy, err := s.legacyImportedRecord(ctx); err != nil {
		return false, err
	} else if legacy != nil {
		s.logger.Warn("MONGORESCUE_API_KEY was already imported by an earlier release; keeping that key",
			slog.String("api_key_id", legacy.ID))
		return false, nil
	}
	id, err := newID("key_")
	if err != nil {
		return false, err
	}
	// The imported key keeps the full rights it had before scopes existed. It was
	// chosen by an administrator, not generated, so it is stored as a keyed MAC
	// rather than a plain digest.
	hash := importedKeyMACScheme + importedKeyMAC(s.importedKeyMACKey, key)
	k := &APIKey{ID: id, Name: importedKeyName, Prefix: prefix, Scope: ScopeAdmin, Hash: hash, CreatedAt: s.now().UTC()}
	if err := s.repo.CreateAPIKey(ctx, k); err != nil {
		return false, err
	}
	s.logger.Info("api key imported", slog.String("api_key_id", k.ID))
	return true, nil
}

// DeleteAPIKey revokes a key.
func (s *Service) DeleteAPIKey(ctx context.Context, actor *Principal, id string) error {
	if err := s.repo.DeleteAPIKey(ctx, id); err != nil {
		return err
	}
	s.logger.Info("api key revoked", slog.String("api_key_id", id), slog.String("by", actor.UserID()))
	return nil
}

// newUser validates and hashes a new account.
func (s *Service) newUser(username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("auth: hash password: %w", err)
	}
	id, err := newID("usr_")
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	return &User{ID: id, Username: username, PasswordHash: string(hash), CreatedAt: now, UpdatedAt: now}, nil
}
