// Package settings holds the runtime configuration that operators manage in the
// dashboard (general limits, security options, backup encryption) and stores in the
// metadata database. Nothing here is read from files or the environment: only the
// bootstrap options in internal/config are.
//
// The package is the domain core: validation, the keep-secret rule for masked values,
// retired encryption keys and the live snapshot the engines, the scheduler and the
// HTTP server read from. Persistence is the Repository port implemented by
// internal/store.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Sentinel errors.
var (
	// ErrInvalid is returned for a setting that fails validation; the message names it.
	ErrInvalid = errors.New("settings: invalid value")
	// ErrMaskedSecret is returned when a masked secret is sent back but nothing is
	// stored under it.
	ErrMaskedSecret = errors.New("settings: secret is masked but none is stored; supply the value")
	// ErrSecretReentry is returned when a secret would be kept for a new
	// destination (another OIDC issuer, another audit webhook host): the secret must
	// be sent again, never the mask, so a stored secret never reaches a host it was
	// not entered for.
	ErrSecretReentry = errors.New("settings: the destination changed; enter the secret again")
)

// SecretMask replaces stored secrets in API responses; sending it back unchanged
// keeps the stored value.
const SecretMask = models.SecretMask

// CookiePolicy decides the Secure attribute of the session cookie.
type CookiePolicy string

// Cookie policies.
const (
	// CookiesAuto sets Secure for TLS requests, or behind a trusted proxy reporting
	// X-Forwarded-Proto: https.
	CookiesAuto CookiePolicy = "auto"
	// CookiesAlways always sets Secure (TLS terminates at a proxy that does not send
	// X-Forwarded-Proto).
	CookiesAlways CookiePolicy = "always"
	// CookiesNever never sets Secure (plain-HTTP test setups only).
	CookiesNever CookiePolicy = "never"
)

// EncryptionMode selects how new backups are encrypted.
type EncryptionMode string

// Encryption modes.
const (
	// ModeX25519 encrypts to age X25519 recipients (public keys).
	ModeX25519 EncryptionMode = "x25519"
	// ModePassphrase encrypts with an scrypt passphrase.
	ModePassphrase EncryptionMode = "passphrase"
)

// Kinds of retired keys.
const (
	// KindX25519 is a retired X25519 identity.
	KindX25519 = "x25519"
	// KindPassphrase is a retired passphrase.
	KindPassphrase = "passphrase"
)

// Validation limits.
const (
	maxRetentionDays    = 36500
	maxRetentionCount   = 100000
	maxCORSOrigins      = 50
	maxRecipients       = 50
	minSessionTimeout   = time.Minute
	maxSessionTimeout   = 365 * 24 * time.Hour
	minPassphraseLength = 16
	maxSecretLength     = 16 << 10
)

// Duration is a time.Duration that is written to and read from JSON as a Go duration
// string ("6h", "90m", "0s").
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler. It accepts duration strings only.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: durations are strings such as \"6h\" or \"90m\"", ErrInvalid)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%w: %q is not a duration such as \"6h\" or \"90m\"", ErrInvalid, s)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Settings is the complete runtime configuration.
type Settings struct {
	// General holds backup and restore defaults and limits.
	General General `json:"general"`
	// Security holds session, cookie, proxy, CORS and metrics options.
	Security Security `json:"security"`
	// Encryption configures age encryption of new backups and the keys restores use.
	Encryption Encryption `json:"encryption"`
	// Integrity configures archive verification, the integrity sweep and storage
	// scans (see integrity.go).
	Integrity Integrity `json:"integrity"`
	// MetadataBackup configures the scheduled self-backup of the metadata database
	// (see metabackup.go).
	MetadataBackup MetadataBackup `json:"metadata_backup"`
	// Audit configures the retention and forwarding of the audit log (see audit.go).
	Audit Audit `json:"audit"`
	// OIDC configures single sign-on through an OpenID Connect provider (see
	// oidc.go).
	OIDC OIDC `json:"oidc"`
	// Monitoring configures the outbound heartbeat (see monitoring.go).
	Monitoring Monitoring `json:"monitoring"`
}

// General holds backup and restore defaults and limits.
type General struct {
	// DefaultRetentionDays pre-fills new jobs (0 keeps backups forever).
	DefaultRetentionDays int `json:"default_retention_days"`
	// DefaultRetentionCount pre-fills new jobs (0 keeps any number).
	DefaultRetentionCount int `json:"default_retention_count"`
	// DefaultGzip compresses new jobs and manual backups unless they say otherwise.
	DefaultGzip bool `json:"default_gzip"`
	// BackupTimeout bounds one backup run (0 = unlimited).
	BackupTimeout Duration `json:"backup_timeout"`
	// BackupStallTimeout aborts a backup whose mongodump is silent this long (0 = off).
	BackupStallTimeout Duration `json:"backup_stall_timeout"`
	// RestoreTimeout bounds one restore run, verification included (0 = unlimited).
	RestoreTimeout Duration `json:"restore_timeout"`
	// RestoreVerifyPolicy decides when restores verify the artifact first.
	RestoreVerifyPolicy models.VerifyPolicy `json:"restore_verify_policy"`
	// LogRetentionDays keeps the log file of every backup and restore run for this
	// many days (0 keeps them until their record is deleted).
	LogRetentionDays int `json:"log_retention_days"`
	// MaxUploadMbps caps the upload of every backup whose job sets no cap of its
	// own, in megabits per second (0 = unlimited).
	MaxUploadMbps float64 `json:"max_upload_mbps"`
	// PostRestoreCommandTimeout bounds each post-restore command of a restore
	// (MinPostRestoreCommandTimeout to MaxPostRestoreCommandTimeout).
	PostRestoreCommandTimeout Duration `json:"post_restore_command_timeout"`
	// StorageStallTimeout fails an upload (backup, oplog chunk, copy, metadata
	// snapshot, re-encryption) whose storage target accepts no bytes for this long
	// (MinStorageStallTimeout to MaxStorageStallTimeout).
	StorageStallTimeout Duration `json:"storage_stall_timeout"`
}

// Limits of General.PostRestoreCommandTimeout.
const (
	MinPostRestoreCommandTimeout = time.Second
	MaxPostRestoreCommandTimeout = 24 * time.Hour
)

// Default and limits of General.StorageStallTimeout.
const (
	DefaultStorageStallTimeout = 5 * time.Minute
	MinStorageStallTimeout     = time.Minute
	MaxStorageStallTimeout     = time.Hour
)

// Security holds session, cookie, proxy, CORS and metrics options.
type Security struct {
	// SessionIdleTimeout ends a session after this long without requests.
	SessionIdleTimeout Duration `json:"session_idle_timeout"`
	// SessionAbsoluteTimeout ends a session this long after login.
	SessionAbsoluteTimeout Duration `json:"session_absolute_timeout"`
	// SecureCookies decides the Secure attribute of the session cookie.
	SecureCookies CookiePolicy `json:"secure_cookies"`
	// TrustProxyHeaders honours X-Forwarded-For / X-Real-IP and X-Forwarded-Proto.
	TrustProxyHeaders bool `json:"trust_proxy_headers"`
	// CORSOrigins lists browser origins allowed to call the API cross-origin.
	CORSOrigins []string `json:"cors_origins"`
	// MetricsPublic serves /metrics without an API key.
	MetricsPublic bool `json:"metrics_public"`
	// MCPEnabled serves the MCP endpoint (/mcp) to API keys; when off it answers 403
	// and the stdio bridge cannot connect.
	MCPEnabled bool `json:"mcp_enabled"`
	// DeleteGraceDays is the delete grace period: deleted backups (by a user, an API
	// key or retention) keep their archive this many days and can be undeleted until
	// then (models.MinDeleteGraceDays to models.MaxDeleteGraceDays). Lowering it takes
	// effect only after the current grace period (see protection.go).
	DeleteGraceDays int `json:"delete_grace_days"`
	// RequireSecondApprover turns on the two-person rule: destructive actions wait
	// for a second administrator's approval. It can only be turned on while at least
	// two administrators exist, and turning it off needs an approval too.
	RequireSecondApprover bool `json:"require_second_approver"`
}

// DeleteGrace returns the delete grace period as a duration.
func (s Security) DeleteGrace() time.Duration {
	return models.GraceDuration(s.DeleteGraceDays)
}

// Encryption configures age encryption.
type Encryption struct {
	// Enabled encrypts new backups.
	Enabled bool `json:"enabled"`
	// Mode selects X25519 recipients or a passphrase.
	Mode EncryptionMode `json:"mode"`
	// Recipients are the X25519 public keys new backups are encrypted to.
	Recipients []string `json:"recipients"`
	// Identity holds X25519 private keys (one per line) used by restores. Secret.
	Identity string `json:"identity"`
	// Passphrase encrypts (passphrase mode) and decrypts scrypt backups. Secret.
	Passphrase string `json:"passphrase"`
	// RetiredKeys are former identities and passphrases, kept so that older backups
	// stay restorable. Their secrets are never serialized.
	RetiredKeys []RetiredKey `json:"retired_keys"`
}

// RetiredKey is a former identity or passphrase.
type RetiredKey struct {
	// Kind is KindX25519 or KindPassphrase.
	Kind string `json:"kind"`
	// Recipient lists the public keys of a retired identity (comma-separated).
	Recipient string `json:"recipient,omitempty"`
	// RetiredAt is when the key was replaced or removed.
	RetiredAt time.Time `json:"retired_at"`
	// Secret is the identity or passphrase itself; it is stored encrypted and never
	// serialized to JSON.
	Secret string `json:"-"`
}

// Defaults returns the settings of a fresh installation.
func Defaults() Settings {
	return Settings{
		General: General{
			DefaultRetentionDays:      30,
			DefaultRetentionCount:     10,
			DefaultGzip:               true,
			BackupTimeout:             Duration(6 * time.Hour),
			BackupStallTimeout:        Duration(10 * time.Minute),
			RestoreTimeout:            Duration(12 * time.Hour),
			RestoreVerifyPolicy:       models.VerifyAuto,
			LogRetentionDays:          30,
			PostRestoreCommandTimeout: Duration(time.Minute),
			StorageStallTimeout:       Duration(DefaultStorageStallTimeout),
		},
		Security: Security{
			SessionIdleTimeout:     Duration(12 * time.Hour),
			SessionAbsoluteTimeout: Duration(7 * 24 * time.Hour),
			SecureCookies:          CookiesAuto,
			CORSOrigins:            []string{},
			MCPEnabled:             true,
			DeleteGraceDays:        models.DefaultDeleteGraceDays,
		},
		Encryption: Encryption{
			Mode:        ModeX25519,
			Recipients:  []string{},
			RetiredKeys: []RetiredKey{},
		},
		Integrity:      defaultIntegrity(),
		MetadataBackup: defaultMetadataBackup(),
		Audit:          defaultAudit(),
		OIDC:           defaultOIDC(),
		Monitoring:     defaultMonitoring(),
	}
}

// Clone returns a deep copy.
func (s Settings) Clone() Settings {
	s.Security.CORSOrigins = slices.Clone(s.Security.CORSOrigins)
	s.Encryption.Recipients = slices.Clone(s.Encryption.Recipients)
	s.Encryption.RetiredKeys = slices.Clone(s.Encryption.RetiredKeys)
	s.OIDC = s.OIDC.clone()
	return s
}

// Masked returns a copy that is safe to serialize to API clients: the identity,
// passphrase, audit webhook secret and OIDC client secret are replaced by
// SecretMask when set, the audit webhook and heartbeat URLs are reduced to their
// origin (retired secrets are never serialized). Nil slices become empty ones.
func (s Settings) Masked() Settings {
	out := s.Clone()
	if out.Encryption.Identity != "" {
		out.Encryption.Identity = SecretMask
	}
	if out.Encryption.Passphrase != "" {
		out.Encryption.Passphrase = SecretMask
	}
	for i := range out.Encryption.RetiredKeys {
		out.Encryption.RetiredKeys[i].Secret = ""
	}
	if out.Security.CORSOrigins == nil {
		out.Security.CORSOrigins = []string{}
	}
	if out.Encryption.Recipients == nil {
		out.Encryption.Recipients = []string{}
	}
	if out.Encryption.RetiredKeys == nil {
		out.Encryption.RetiredKeys = []RetiredKey{}
	}
	out.Audit = out.Audit.masked()
	out.OIDC = out.OIDC.masked()
	out.Monitoring = out.Monitoring.masked()
	return out
}

// Patch is a partial update: nil groups and fields keep their current values.
type Patch struct {
	// General updates general settings.
	General *GeneralPatch `json:"general,omitempty"`
	// Security updates security settings.
	Security *SecurityPatch `json:"security,omitempty"`
	// Encryption updates encryption settings.
	Encryption *EncryptionPatch `json:"encryption,omitempty"`
	// Integrity updates the integrity settings.
	Integrity *IntegrityPatch `json:"integrity,omitempty"`
	// MetadataBackup updates the metadata backup settings.
	MetadataBackup *MetadataBackupPatch `json:"metadata_backup,omitempty"`
	// Audit updates the audit log settings.
	Audit *AuditPatch `json:"audit,omitempty"`
	// OIDC updates the single sign-on settings.
	OIDC *OIDCPatch `json:"oidc,omitempty"`
	// Monitoring updates the heartbeat settings.
	Monitoring *MonitoringPatch `json:"monitoring,omitempty"`
}

// GeneralPatch updates General; see General for the fields.
type GeneralPatch struct {
	DefaultRetentionDays      *int                 `json:"default_retention_days,omitempty"`
	DefaultRetentionCount     *int                 `json:"default_retention_count,omitempty"`
	DefaultGzip               *bool                `json:"default_gzip,omitempty"`
	BackupTimeout             *Duration            `json:"backup_timeout,omitempty"`
	BackupStallTimeout        *Duration            `json:"backup_stall_timeout,omitempty"`
	RestoreTimeout            *Duration            `json:"restore_timeout,omitempty"`
	RestoreVerifyPolicy       *models.VerifyPolicy `json:"restore_verify_policy,omitempty"`
	LogRetentionDays          *int                 `json:"log_retention_days,omitempty"`
	MaxUploadMbps             *float64             `json:"max_upload_mbps,omitempty"`
	PostRestoreCommandTimeout *Duration            `json:"post_restore_command_timeout,omitempty"`
	StorageStallTimeout       *Duration            `json:"storage_stall_timeout,omitempty"`
}

// SecurityPatch updates Security; see Security for the fields.
type SecurityPatch struct {
	SessionIdleTimeout     *Duration     `json:"session_idle_timeout,omitempty"`
	SessionAbsoluteTimeout *Duration     `json:"session_absolute_timeout,omitempty"`
	SecureCookies          *CookiePolicy `json:"secure_cookies,omitempty"`
	TrustProxyHeaders      *bool         `json:"trust_proxy_headers,omitempty"`
	CORSOrigins            *[]string     `json:"cors_origins,omitempty"`
	MetricsPublic          *bool         `json:"metrics_public,omitempty"`
	MCPEnabled             *bool         `json:"mcp_enabled,omitempty"`
	DeleteGraceDays        *int          `json:"delete_grace_days,omitempty"`
	RequireSecondApprover  *bool         `json:"require_second_approver,omitempty"`
}

// EncryptionPatch updates Encryption. Identity and Passphrase follow the keep-secret
// rule: SecretMask keeps the stored value, "" removes it and any other value replaces
// it; a removed or replaced secret is retired, not forgotten.
type EncryptionPatch struct {
	Enabled    *bool           `json:"enabled,omitempty"`
	Mode       *EncryptionMode `json:"mode,omitempty"`
	Recipients *[]string       `json:"recipients,omitempty"`
	Identity   *string         `json:"identity,omitempty"`
	Passphrase *string         `json:"passphrase,omitempty"`
}

// apply returns cur with p applied (secrets resolved against cur). Replaced or removed
// secrets are moved to the retired keys.
func (p Patch) apply(cur Settings, now time.Time) (Settings, error) {
	next := cur.Clone()
	if g := p.General; g != nil {
		setIf(&next.General.DefaultRetentionDays, g.DefaultRetentionDays)
		setIf(&next.General.DefaultRetentionCount, g.DefaultRetentionCount)
		setIf(&next.General.DefaultGzip, g.DefaultGzip)
		setIf(&next.General.BackupTimeout, g.BackupTimeout)
		setIf(&next.General.BackupStallTimeout, g.BackupStallTimeout)
		setIf(&next.General.RestoreTimeout, g.RestoreTimeout)
		setIf(&next.General.RestoreVerifyPolicy, g.RestoreVerifyPolicy)
		setIf(&next.General.LogRetentionDays, g.LogRetentionDays)
		setIf(&next.General.MaxUploadMbps, g.MaxUploadMbps)
		setIf(&next.General.PostRestoreCommandTimeout, g.PostRestoreCommandTimeout)
		setIf(&next.General.StorageStallTimeout, g.StorageStallTimeout)
	}
	if sec := p.Security; sec != nil {
		setIf(&next.Security.SessionIdleTimeout, sec.SessionIdleTimeout)
		setIf(&next.Security.SessionAbsoluteTimeout, sec.SessionAbsoluteTimeout)
		setIf(&next.Security.SecureCookies, sec.SecureCookies)
		setIf(&next.Security.TrustProxyHeaders, sec.TrustProxyHeaders)
		if sec.CORSOrigins != nil {
			next.Security.CORSOrigins = slices.Clone(*sec.CORSOrigins)
		}
		setIf(&next.Security.MetricsPublic, sec.MetricsPublic)
		setIf(&next.Security.MCPEnabled, sec.MCPEnabled)
		setIf(&next.Security.DeleteGraceDays, sec.DeleteGraceDays)
		setIf(&next.Security.RequireSecondApprover, sec.RequireSecondApprover)
	}
	if enc := p.Encryption; enc != nil {
		setIf(&next.Encryption.Enabled, enc.Enabled)
		setIf(&next.Encryption.Mode, enc.Mode)
		if enc.Recipients != nil {
			next.Encryption.Recipients = slices.Clone(*enc.Recipients)
		}
		if enc.Identity != nil {
			v, err := keepSecret(strings.TrimSpace(*enc.Identity), cur.Encryption.Identity)
			if err != nil {
				return cur, fmt.Errorf("encryption.identity: %w", err)
			}
			next.Encryption.Identity = v
		}
		if enc.Passphrase != nil {
			v, err := keepSecret(*enc.Passphrase, cur.Encryption.Passphrase)
			if err != nil {
				return cur, fmt.Errorf("encryption.passphrase: %w", err)
			}
			next.Encryption.Passphrase = v
		}
		next.Encryption.RetiredKeys = retire(next.Encryption.RetiredKeys, cur.Encryption, next.Encryption, now)
	}
	p.Integrity.apply(&next.Integrity)
	p.MetadataBackup.apply(&next.MetadataBackup)
	if err := p.Audit.apply(&next.Audit); err != nil {
		return cur, err
	}
	if err := p.OIDC.apply(&next.OIDC); err != nil {
		return cur, err
	}
	if err := p.Monitoring.apply(&next.Monitoring); err != nil {
		return cur, err
	}
	return next, nil
}

// setIf assigns *src to *dst when src is non-nil.
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// keepSecret resolves an incoming secret: SecretMask keeps stored (which must exist).
func keepSecret(incoming, stored string) (string, error) {
	if incoming != SecretMask {
		return incoming, nil
	}
	if stored == "" {
		return "", ErrMaskedSecret
	}
	return stored, nil
}

// retire moves secrets of prev that next no longer uses into the retired list, and
// drops retired entries that became active again.
func retire(retired []RetiredKey, prev, next Encryption, now time.Time) []RetiredKey {
	out := slices.DeleteFunc(slices.Clone(retired), func(k RetiredKey) bool {
		return (k.Kind == KindX25519 && k.Secret == next.Identity) || (k.Kind == KindPassphrase && k.Secret == next.Passphrase)
	})
	known := func(kind, secret string) bool {
		return slices.ContainsFunc(out, func(k RetiredKey) bool { return k.Kind == kind && k.Secret == secret })
	}
	if prev.Identity != "" && prev.Identity != next.Identity && !known(KindX25519, prev.Identity) {
		out = append(out, RetiredKey{Kind: KindX25519, Recipient: recipientsOf(prev.Identity), RetiredAt: now.UTC(), Secret: prev.Identity})
	}
	if prev.Passphrase != "" && prev.Passphrase != next.Passphrase && !known(KindPassphrase, prev.Passphrase) {
		out = append(out, RetiredKey{Kind: KindPassphrase, RetiredAt: now.UTC(), Secret: prev.Passphrase})
	}
	return out
}

// validate normalises s and checks every value. strictPassphrase enforces the minimum
// passphrase length (not applied to values imported from older releases).
func validate(s *Settings, strictPassphrase bool) error {
	g := &s.General
	switch {
	case g.DefaultRetentionDays < 0 || g.DefaultRetentionDays > maxRetentionDays:
		return fmt.Errorf("%w: general.default_retention_days must be between 0 and %d", ErrInvalid, maxRetentionDays)
	case g.DefaultRetentionCount < 0 || g.DefaultRetentionCount > maxRetentionCount:
		return fmt.Errorf("%w: general.default_retention_count must be between 0 and %d", ErrInvalid, maxRetentionCount)
	case g.BackupTimeout < 0:
		return fmt.Errorf("%w: general.backup_timeout must not be negative (0 disables it)", ErrInvalid)
	case g.BackupStallTimeout < 0:
		return fmt.Errorf("%w: general.backup_stall_timeout must not be negative (0 disables it)", ErrInvalid)
	case g.RestoreTimeout < 0:
		return fmt.Errorf("%w: general.restore_timeout must not be negative (0 disables it)", ErrInvalid)
	case !g.RestoreVerifyPolicy.Valid():
		return fmt.Errorf("%w: general.restore_verify_policy must be always, auto or never", ErrInvalid)
	case g.LogRetentionDays < 0 || g.LogRetentionDays > maxRetentionDays:
		return fmt.Errorf("%w: general.log_retention_days must be between 0 and %d", ErrInvalid, maxRetentionDays)
	case models.ValidateUploadMbps(g.MaxUploadMbps) != nil:
		return fmt.Errorf("%w: general.%w", ErrInvalid, models.ErrInvalidUploadRate)
	case g.PostRestoreCommandTimeout.Std() < MinPostRestoreCommandTimeout || g.PostRestoreCommandTimeout.Std() > MaxPostRestoreCommandTimeout:
		return fmt.Errorf("%w: general.post_restore_command_timeout must be between 1s and 24h", ErrInvalid)
	case g.StorageStallTimeout.Std() < MinStorageStallTimeout || g.StorageStallTimeout.Std() > MaxStorageStallTimeout:
		return fmt.Errorf("%w: general.storage_stall_timeout must be between 1m and 1h", ErrInvalid)
	}

	sec := &s.Security
	switch {
	case sec.SessionIdleTimeout.Std() < minSessionTimeout || sec.SessionIdleTimeout.Std() > maxSessionTimeout:
		return fmt.Errorf("%w: security.session_idle_timeout must be between 1m and 8760h", ErrInvalid)
	case sec.SessionAbsoluteTimeout.Std() < minSessionTimeout || sec.SessionAbsoluteTimeout.Std() > maxSessionTimeout:
		return fmt.Errorf("%w: security.session_absolute_timeout must be between 1m and 8760h", ErrInvalid)
	case sec.SessionAbsoluteTimeout < sec.SessionIdleTimeout:
		return fmt.Errorf("%w: security.session_absolute_timeout must not be shorter than session_idle_timeout", ErrInvalid)
	case sec.DeleteGraceDays < models.MinDeleteGraceDays || sec.DeleteGraceDays > models.MaxDeleteGraceDays:
		return fmt.Errorf("%w: security.delete_grace_days must be between %d and %d", ErrInvalid, models.MinDeleteGraceDays, models.MaxDeleteGraceDays)
	}
	switch sec.SecureCookies {
	case CookiesAuto, CookiesAlways, CookiesNever:
	default:
		return fmt.Errorf("%w: security.secure_cookies must be auto, always or never", ErrInvalid)
	}
	origins, err := normalizeOrigins(sec.CORSOrigins)
	if err != nil {
		return err
	}
	sec.CORSOrigins = origins

	if err := validateIntegrity(&s.Integrity); err != nil {
		return err
	}
	if err := validateMetadataBackup(&s.MetadataBackup); err != nil {
		return err
	}
	if err := validateAudit(&s.Audit); err != nil {
		return err
	}
	if err := validateOIDC(&s.OIDC); err != nil {
		return err
	}
	if err := validateMonitoring(&s.Monitoring); err != nil {
		return err
	}
	return validateEncryption(&s.Encryption, strictPassphrase)
}

// normalizeOrigins trims, de-duplicates and checks CORS origins: each must be
// "scheme://host[:port]" with scheme http or https and nothing else.
func normalizeOrigins(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		o := strings.TrimRight(strings.TrimSpace(raw), "/")
		if o == "" || slices.Contains(out, o) {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || o != u.Scheme+"://"+u.Host {
			return nil, fmt.Errorf("%w: security.cors_origins: %q is not an origin such as https://ops.example.com", ErrInvalid, truncate(o, 80))
		}
		out = append(out, o)
	}
	if len(out) > maxCORSOrigins {
		return nil, fmt.Errorf("%w: security.cors_origins allows at most %d entries", ErrInvalid, maxCORSOrigins)
	}
	return out, nil
}

// truncate shortens s for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
