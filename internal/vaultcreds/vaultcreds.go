// Package vaultcreds resolves the credentials of connections and S3 storage
// targets that are read from HashiCorp Vault (models.CredentialsVault) instead
// of being stored.
//
// Credentials are resolved when they are used (a backup, a restore, a test, the
// oplog collection), never when a connection or target is saved, and nothing
// read from Vault is ever written to the store or logged: errors name the Vault
// path and the field, never a value.
//
// Static kv-v2 values are cached for a short TTL (Config.CacheTTL). Dynamic
// credentials (the database and AWS secrets engines) carry a lease: a lease is
// shared by the runs of one connection while it stays fresh, renewed while a run
// holds it (Hold) and revoked, best effort, when the last run releases it. When
// a lease can no longer be renewed (Vault is unreachable until the lease nearly
// ends, or the role's maximum TTL is reached) the contexts of its holders end
// with a cause wrapping ErrLeaseLost, so a run fails clearly and a long-lived
// session reconnects with fresh credentials. There is no fallback to any stored
// copy: when Vault cannot be reached, the use fails and Config.Alert is called.
package vaultcreds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/vault"
)

// DefaultCacheTTL is how long a static kv-v2 value is reused before Vault is
// read again.
const DefaultCacheTTL = 5 * time.Minute

// MaxCacheTTL bounds Config.CacheTTL.
const MaxCacheTTL = 24 * time.Hour

// Timing of leases, as fractions of the lease's TTL.
const (
	// renewAt: a held, renewable lease is renewed once a third of its TTL is
	// left.
	renewFraction = 3
	// freshFraction: a lease with less than a quarter of its TTL left is not
	// handed to a new use; a new one is issued.
	freshFraction = 4
	// marginFraction: a fifth of the TTL is the safety margin. A lease that
	// cannot be renewed ends for its holders this long before it expires.
	marginFraction = 5
	// maxMargin caps the margin of long leases.
	maxMargin = 5 * time.Minute
	// idleGrace keeps a lease no run holds for this long after it was last
	// handed out, so a run that resolved it but did not hold it yet keeps working.
	idleGrace = time.Minute
	// alertEvery repeats the alert of a failure episode at most this often.
	alertEvery = time.Hour
	// revokeTimeout bounds a best-effort revocation.
	revokeTimeout = 10 * time.Second
)

// Sentinel errors. Every resolution error wraps one of them and names the Vault
// path, never a value.
var (
	// ErrNotConfigured is returned when credentials from Vault are used but no
	// Vault is configured.
	ErrNotConfigured = errors.New("credentials from Vault are not configured: set MONGORESCUE_VAULT_ADDR and a Vault authentication (" +
		vault.EnvToken + ", AppRole or Kubernetes) and restart")
	// ErrUnavailable is returned when Vault cannot be reached or answered with a
	// server error (sealed, overloaded).
	ErrUnavailable = errors.New("vault is unavailable")
	// ErrRefused is returned when Vault refused the request (permission denied,
	// a missing secret or role, a failed login).
	ErrRefused = errors.New("vault refused the request")
	// ErrInvalidSecret is returned when the secret lacks the named fields or
	// they are not strings.
	ErrInvalidSecret = errors.New("the vault secret does not hold the expected credentials")
	// ErrLeaseLost is the cause of a holder's context when the lease of its
	// credentials ends: renewal failed until the lease nearly expired, or the
	// lease reached its maximum TTL.
	ErrLeaseLost = errors.New("the vault lease of the credentials could not be kept valid")
)

// Config configures a Resolver.
type Config struct {
	// Client calls Vault; nil makes every resolution fail with ErrNotConfigured.
	Client *vault.Client
	// CacheTTL is how long static kv-v2 values are reused (DefaultCacheTTL when
	// zero; a negative value disables the cache).
	CacheTTL time.Duration
	// Logger receives warnings (never a secret); nil uses slog.Default().
	Logger *slog.Logger
	// Alert is called, with a redacted message, on the first failure of an
	// episode of failed resolutions or renewals and at most hourly while it
	// lasts; Recovered when a resolution succeeds again. Both may be nil.
	Alert     func(message string)
	Recovered func()
}

// UserPassword is the user name and password of a connection.
type UserPassword struct {
	// Username and Password authenticate to MongoDB.
	Username, Password string
}

// Resolver resolves credentials from Vault. It is safe for concurrent use; Close
// stops its renewals.
type Resolver struct {
	client   *vault.Client
	cacheTTL time.Duration
	logger   *slog.Logger
	alert    func(string)
	recover  func()
	// unit is the length of one second of a lease duration (tests shrink it).
	unit time.Duration
	now  func() time.Time

	// ctx bounds the renewals and revocations; cancel ends them (Close).
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	static    map[string]staticEntry
	leases    map[string]*lease
	keyLocks  map[string]*sync.Mutex
	s3Leases  map[string]time.Time
	closed    bool
	failing   bool
	alertedAt time.Time
}

// staticEntry is a cached kv-v2 read.
type staticEntry struct {
	values  map[string]string
	fetched time.Time
}

// New returns a Resolver of cfg.
func New(cfg Config) *Resolver {
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{client: cfg.Client, cacheTTL: ttl, logger: logger, alert: cfg.Alert, recover: cfg.Recovered,
		unit: time.Second, now: time.Now, ctx: ctx, cancel: cancel,
		static: map[string]staticEntry{}, leases: map[string]*lease{}, keyLocks: map[string]*sync.Mutex{}, s3Leases: map[string]time.Time{}}
}

// Configured reports whether a Vault client is set.
func (r *Resolver) Configured() bool { return r != nil && r.client != nil }

// SetAlert replaces Config.Alert (the alert's channel may be built after the
// resolver).
func (r *Resolver) SetAlert(alert func(message string)) {
	r.mu.Lock()
	r.alert = alert
	r.mu.Unlock()
}

// Check verifies that Vault answers and accepts MongoRescue's authentication
// (a login, then a lookup of the token).
func (r *Resolver) Check(ctx context.Context) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := r.client.Do(ctx, http.MethodGet, "/v1/auth/token/lookup-self", nil, nil); err != nil {
		return classify("the token of MongoRescue", err)
	}
	return nil
}

// Close stops every renewal, ends the contexts of the remaining holders and
// revokes the leases still open, best effort. It waits for the renewals to stop.
func (r *Resolver) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	var ids []string
	for _, l := range r.leases {
		l.endLocked(fmt.Errorf("%w: MongoRescue is shutting down", ErrLeaseLost))
		ids = append(ids, l.id)
	}
	for id, exp := range r.s3Leases {
		if exp.After(r.now()) {
			ids = append(ids, id)
		}
	}
	r.leases, r.s3Leases, r.static = map[string]*lease{}, map[string]time.Time{}, map[string]staticEntry{}
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
	if r.client == nil {
		return
	}
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		r.revoke(ctx, id)
		cancel()
	}
}

// Connection returns the user name and password ref names, for a use that does
// not outlive the lease's freshness (a test, a listing, the preparation of a
// run). A run holds them with Hold instead.
func (r *Resolver) Connection(ctx context.Context, ref models.VaultRef) (UserPassword, error) {
	if err := r.ready(); err != nil {
		return UserPassword{}, err
	}
	switch ref.Engine {
	case models.VaultKV2:
		vals, err := r.kv(ctx, ref, []string{ref.UsernameField, ref.PasswordField})
		if err != nil {
			return UserPassword{}, r.failed(err)
		}
		r.succeeded()
		return UserPassword{Username: vals[ref.UsernameField], Password: vals[ref.PasswordField]}, nil
	case models.VaultDatabase:
		l, err := r.shared(ctx, ref, true)
		if err != nil {
			return UserPassword{}, r.failed(err)
		}
		r.succeeded()
		return UserPassword{Username: l.values["username"], Password: l.values["password"]}, nil
	}
	return UserPassword{}, fmt.Errorf("%w: engine %q does not supply connection credentials", models.ErrInvalidVaultRef, ref.Engine)
}

// Hold returns the credentials of ref for a run and keeps them valid until
// release is called: a dynamic lease is renewed meanwhile and revoked, best
// effort, once no run holds it. The returned context is derived from ctx and
// ends with a cause wrapping ErrLeaseLost when the lease can no longer be kept
// valid. Static kv-v2 credentials are returned as Connection returns them, with
// ctx itself. release is idempotent.
func (r *Resolver) Hold(ctx context.Context, ref models.VaultRef) (context.Context, UserPassword, func(), error) {
	if !ref.Dynamic() {
		up, err := r.Connection(ctx, ref)
		return ctx, up, func() {}, err
	}
	if err := r.ready(); err != nil {
		return ctx, UserPassword{}, func() {}, err
	}
	for attempt := 0; ; attempt++ {
		l, err := r.shared(ctx, ref, false)
		if err != nil {
			return ctx, UserPassword{}, func() {}, r.failed(err)
		}
		r.mu.Lock()
		if l.ended != nil || r.closed {
			// The lease ended between the lookup and now: take a new one.
			r.mu.Unlock()
			if attempt == 0 && !r.isClosed() {
				continue
			}
			return ctx, UserPassword{}, func() {}, r.failed(fmt.Errorf("%w: %s", ErrLeaseLost, ref.String()))
		}
		hctx, cancel := context.WithCancelCause(ctx)
		h := &holder{cancel: cancel, since: r.now()}
		l.holders[h] = struct{}{}
		if !l.watching {
			l.watching = true
			l.stop = make(chan struct{})
			r.wg.Add(1)
			go r.keep(l, l.stop)
		}
		up := UserPassword{Username: l.values["username"], Password: l.values["password"]}
		r.mu.Unlock()
		r.succeeded()
		var once sync.Once
		release := func() {
			once.Do(func() {
				r.release(l, h)
				cancel(context.Canceled)
			})
		}
		return hctx, up, release, nil
	}
}

// isClosed reports whether Close ran.
func (r *Resolver) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// ready returns ErrNotConfigured without a client.
func (r *Resolver) ready() error {
	if !r.Configured() {
		return ErrNotConfigured
	}
	return nil
}

// failed reports err (alerting on the first failure of an episode) and returns
// it. Invalid references are configuration errors, not an episode.
func (r *Resolver) failed(err error) error {
	if errors.Is(err, models.ErrInvalidVaultRef) || errors.Is(err, context.Canceled) {
		return err
	}
	msg := redact.Text(err.Error())
	r.mu.Lock()
	now := r.now()
	alert := !r.failing || now.Sub(r.alertedAt) >= alertEvery
	r.failing = true
	if alert {
		r.alertedAt = now
	}
	hook := r.alert
	r.mu.Unlock()
	if alert {
		r.logger.Warn("credentials from Vault could not be resolved", logsafe.Attr("error", msg))
		if hook != nil {
			hook(msg)
		}
	}
	return err
}

// succeeded ends a failure episode.
func (r *Resolver) succeeded() {
	r.mu.Lock()
	was := r.failing
	r.failing = false
	r.mu.Unlock()
	if was {
		r.logger.Info("credentials from Vault resolve again")
		if r.recover != nil {
			r.recover()
		}
	}
}

// classify wraps an error of the Vault client into ErrUnavailable or ErrRefused,
// with what was read and a hint.
func classify(what string, err error) error {
	var se *vault.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, vault.ErrAuthConfig):
		return fmt.Errorf("%w: %s: %w; check the Vault authentication of MongoRescue", ErrRefused, what, err)
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		return fmt.Errorf("%w: %s: %w; check that the policy of MongoRescue's Vault token allows reading it", ErrRefused, what, err)
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %s: %w; check the mount, path or role", ErrRefused, what, err)
	case errors.As(err, &se) && se.Permanent():
		return fmt.Errorf("%w: %s: %w", ErrRefused, what, err)
	}
	return fmt.Errorf("%w: %s: %w; check that Vault is reachable and unsealed", ErrUnavailable, what, err)
}

// kv returns fields of the kv-v2 secret of ref, from the cache while fresh.
func (r *Resolver) kv(ctx context.Context, ref models.VaultRef, fields []string) (map[string]string, error) {
	key := ref.Key()
	r.mu.Lock()
	e, ok := r.static[key]
	r.mu.Unlock()
	if ok && r.cacheTTL > 0 && r.now().Sub(e.fetched) < r.cacheTTL {
		return e.values, nil
	}
	vals, _, err := r.readKV(ctx, ref, fields)
	if err != nil {
		return nil, err
	}
	if r.cacheTTL > 0 {
		r.mu.Lock()
		if !r.closed {
			r.static[key] = staticEntry{values: vals, fetched: r.now()}
		}
		r.mu.Unlock()
	}
	return vals, nil
}

// readKV reads fields of the kv-v2 secret of ref from Vault, and its version.
func (r *Resolver) readKV(ctx context.Context, ref models.VaultRef, fields []string) (map[string]string, int, error) {
	path := "/v1/" + ref.Mount + "/data/" + ref.Path
	var out struct {
		Data struct {
			Data     map[string]any `json:"data"`
			Metadata struct {
				Version      int    `json:"version"`
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := r.client.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, 0, classify(ref.String(), err)
	}
	if out.Data.Data == nil {
		return nil, 0, fmt.Errorf("%w: %s has no current version (deleted or destroyed)", ErrInvalidSecret, ref.String())
	}
	vals, err := pick(ref, out.Data.Data, fields)
	return vals, out.Data.Metadata.Version, err
}

// pick returns the string values of fields in data.
func pick(ref models.VaultRef, data map[string]any, fields []string) (map[string]string, error) {
	vals := make(map[string]string, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		raw, ok := data[f]
		if !ok || raw == nil {
			return nil, fmt.Errorf("%w: %s has no field %q", ErrInvalidSecret, ref.String(), f)
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("%w: field %q of %s is not a non-empty string", ErrInvalidSecret, f, ref.String())
		}
		vals[f] = s
	}
	return vals, nil
}

// Fields returns the names of the fields a reference reads, sorted.
func Fields(ref models.VaultRef) []string {
	var out []string
	for _, f := range []string{ref.UsernameField, ref.PasswordField, ref.AccessKeyField, ref.SecretKeyField, ref.SessionTokenField} {
		if f != "" {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
