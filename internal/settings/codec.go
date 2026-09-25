package settings

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Setting keys as stored in the database (one row per key, JSON-encoded value).
const (
	KeyDefaultRetentionDays   = "general.default_retention_days"
	KeyDefaultRetentionCount  = "general.default_retention_count"
	KeyDefaultGzip            = "general.default_gzip"
	KeyBackupTimeout          = "general.backup_timeout"
	KeyBackupStallTimeout     = "general.backup_stall_timeout"
	KeyRestoreTimeout         = "general.restore_timeout"
	KeyRestoreVerifyPolicy    = "general.restore_verify_policy"
	KeySessionIdleTimeout     = "security.session_idle_timeout"
	KeySessionAbsoluteTimeout = "security.session_absolute_timeout"
	KeySecureCookies          = "security.secure_cookies"
	KeyTrustProxyHeaders      = "security.trust_proxy_headers"
	KeyCORSOrigins            = "security.cors_origins"
	KeyMetricsPublic          = "security.metrics_public"
	KeyEncryptionEnabled      = "encryption.enabled"
	KeyEncryptionMode         = "encryption.mode"
	KeyEncryptionRecipients   = "encryption.recipients"
	KeyEncryptionIdentity     = "encryption.identity"
	KeyEncryptionPassphrase   = "encryption.passphrase"
	KeyEncryptionRetiredKeys  = "encryption.retired_keys"
)

// markerPrefix prefixes the keys recording one-time imports of deprecated
// environment variables and legacy configuration files.
const markerPrefix = "legacy_import."

// keyDef maps one setting key to its field.
type keyDef struct {
	name   string
	secret bool
	get    func(*Settings) any
	set    func(*Settings, []byte) error
}

// field builds a keyDef for the field ptr points to.
func field[T any](name string, secret bool, ptr func(*Settings) *T) keyDef {
	return keyDef{
		name:   name,
		secret: secret,
		get:    func(s *Settings) any { return *ptr(s) },
		set:    func(s *Settings, raw []byte) error { return json.Unmarshal(raw, ptr(s)) },
	}
}

// storedRetiredKey is the stored form of a RetiredKey, secret included (the whole
// value is sealed by the repository).
type storedRetiredKey struct {
	Kind      string    `json:"kind"`
	Recipient string    `json:"recipient,omitempty"`
	RetiredAt time.Time `json:"retired_at"`
	Secret    string    `json:"secret"`
}

var keyDefs = []keyDef{
	field(KeyDefaultRetentionDays, false, func(s *Settings) *int { return &s.General.DefaultRetentionDays }),
	field(KeyDefaultRetentionCount, false, func(s *Settings) *int { return &s.General.DefaultRetentionCount }),
	field(KeyDefaultGzip, false, func(s *Settings) *bool { return &s.General.DefaultGzip }),
	field(KeyBackupTimeout, false, func(s *Settings) *Duration { return &s.General.BackupTimeout }),
	field(KeyBackupStallTimeout, false, func(s *Settings) *Duration { return &s.General.BackupStallTimeout }),
	field(KeyRestoreTimeout, false, func(s *Settings) *Duration { return &s.General.RestoreTimeout }),
	field(KeyRestoreVerifyPolicy, false, func(s *Settings) *string { return (*string)(&s.General.RestoreVerifyPolicy) }),
	field(KeySessionIdleTimeout, false, func(s *Settings) *Duration { return &s.Security.SessionIdleTimeout }),
	field(KeySessionAbsoluteTimeout, false, func(s *Settings) *Duration { return &s.Security.SessionAbsoluteTimeout }),
	field(KeySecureCookies, false, func(s *Settings) *CookiePolicy { return &s.Security.SecureCookies }),
	field(KeyTrustProxyHeaders, false, func(s *Settings) *bool { return &s.Security.TrustProxyHeaders }),
	field(KeyCORSOrigins, false, func(s *Settings) *[]string { return &s.Security.CORSOrigins }),
	field(KeyMetricsPublic, false, func(s *Settings) *bool { return &s.Security.MetricsPublic }),
	field(KeyEncryptionEnabled, false, func(s *Settings) *bool { return &s.Encryption.Enabled }),
	field(KeyEncryptionMode, false, func(s *Settings) *EncryptionMode { return &s.Encryption.Mode }),
	field(KeyEncryptionRecipients, false, func(s *Settings) *[]string { return &s.Encryption.Recipients }),
	field(KeyEncryptionIdentity, true, func(s *Settings) *string { return &s.Encryption.Identity }),
	field(KeyEncryptionPassphrase, true, func(s *Settings) *string { return &s.Encryption.Passphrase }),
	{
		name:   KeyEncryptionRetiredKeys,
		secret: true,
		get: func(s *Settings) any {
			out := make([]storedRetiredKey, 0, len(s.Encryption.RetiredKeys))
			for _, k := range s.Encryption.RetiredKeys {
				out = append(out, storedRetiredKey(k))
			}
			return out
		},
		set: func(s *Settings, raw []byte) error {
			var stored []storedRetiredKey
			if err := json.Unmarshal(raw, &stored); err != nil {
				return err
			}
			s.Encryption.RetiredKeys = make([]RetiredKey, 0, len(stored))
			for _, k := range stored {
				s.Encryption.RetiredKeys = append(s.Encryption.RetiredKeys, RetiredKey(k))
			}
			return nil
		},
	},
}

// Keys returns every setting key.
func Keys() []string {
	out := make([]string, 0, len(keyDefs))
	for _, d := range keyDefs {
		out = append(out, d.name)
	}
	return out
}

// IsSecret reports whether the value stored under key is a secret that the repository
// must encrypt.
func IsSecret(key string) bool {
	return slices.ContainsFunc(keyDefs, func(d keyDef) bool { return d.name == key && d.secret })
}

// IsKnown reports whether key is a setting key or an import marker.
func IsKnown(key string) bool {
	return strings.HasPrefix(key, markerPrefix) || slices.ContainsFunc(keyDefs, func(d keyDef) bool { return d.name == key })
}

// encode returns the stored form of every key of s.
func encode(s Settings) (map[string]string, error) {
	out := make(map[string]string, len(keyDefs))
	for _, d := range keyDefs {
		raw, err := json.Marshal(d.get(&s))
		if err != nil {
			return nil, fmt.Errorf("settings: encode %s: %w", d.name, err)
		}
		out[d.name] = string(raw)
	}
	return out, nil
}

// decode builds Settings from stored values, using Defaults for missing keys.
func decode(values map[string]string) (Settings, error) {
	s := Defaults()
	for _, d := range keyDefs {
		raw, ok := values[d.name]
		if !ok {
			continue
		}
		if err := d.set(&s, []byte(raw)); err != nil {
			return Settings{}, fmt.Errorf("settings: stored value of %s is invalid: %w", d.name, err)
		}
	}
	return s, nil
}
