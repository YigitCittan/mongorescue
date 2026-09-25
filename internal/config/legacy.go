package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// LegacyFileName is the configuration file of earlier releases that is imported from
// the data directory.
const LegacyFileName = "config.json"

// maxLegacyFileSize bounds the legacy files read (config.json, identity files).
const maxLegacyFileSize = 1 << 20

// Deprecated environment variables of earlier releases. They are imported into the
// database once and ignored afterwards.
const (
	EnvAPIKey             = "MONGORESCUE_API_KEY" //nolint:gosec // G101: a variable name, not a credential.
	EnvMongoURI           = "MONGORESCUE_MONGO_URI"
	EnvDBPath             = "MONGORESCUE_DB_PATH"
	EnvStorageType        = "MONGORESCUE_STORAGE_TYPE"
	EnvLocalPath          = "MONGORESCUE_LOCAL_PATH"
	EnvS3Bucket           = "AWS_S3_BUCKET"
	EnvS3Endpoint         = "AWS_S3_ENDPOINT"
	EnvS3PathStyle        = "AWS_S3_USE_PATH_STYLE"
	EnvCORSOrigins        = "MONGORESCUE_CORS_ORIGINS"
	EnvMetricsPublic      = "MONGORESCUE_METRICS_PUBLIC"
	EnvTrustProxy         = "MONGORESCUE_TRUST_PROXY_HEADERS"
	EnvSecureCookies      = "MONGORESCUE_SECURE_COOKIES"
	EnvBackupTimeout      = "MONGORESCUE_BACKUP_TIMEOUT"
	EnvBackupStall        = "MONGORESCUE_BACKUP_STALL_TIMEOUT"
	EnvRestoreTimeout     = "MONGORESCUE_RESTORE_TIMEOUT"
	EnvRestoreVerify      = "MONGORESCUE_RESTORE_VERIFY"
	EnvEncryptionEnabled  = "MONGORESCUE_ENCRYPTION_ENABLED"
	EnvEncryptionRecips   = "MONGORESCUE_ENCRYPTION_RECIPIENTS"
	EnvEncryptionIdentity = "MONGORESCUE_ENCRYPTION_IDENTITY"
	EnvEncryptionIDFile   = "MONGORESCUE_ENCRYPTION_IDENTITY_FILE"
	EnvEncryptionPass     = "MONGORESCUE_ENCRYPTION_PASSPHRASE" //nolint:gosec // G101: a variable name, not a credential.
)

// Standard AWS variables that earlier releases used for the S3 storage. They are only
// imported together with an S3 storage configuration and never reported as
// deprecated, since the AWS SDK itself honours them.
const (
	envAWSRegion    = "AWS_REGION"
	envAWSAccessKey = "AWS_ACCESS_KEY_ID"
	envAWSSecretKey = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // G101: a variable name, not a credential.
)

// deprecatedEnv lists every deprecated variable, for the "ignored" warnings.
var deprecatedEnv = []string{
	EnvAPIKey, EnvMongoURI, EnvDBPath, EnvStorageType, EnvLocalPath, EnvS3Bucket, EnvS3Endpoint,
	EnvS3PathStyle, EnvCORSOrigins, EnvMetricsPublic, EnvTrustProxy, EnvSecureCookies, EnvBackupTimeout,
	EnvBackupStall, EnvRestoreTimeout, EnvRestoreVerify, EnvEncryptionEnabled, EnvEncryptionRecips,
	EnvEncryptionIdentity, EnvEncryptionIDFile, EnvEncryptionPass,
}

// Legacy is the configuration found in deprecated sources.
type Legacy struct {
	// Settings are values for the settings service, in import order.
	Settings []settings.Import
	// Storage is the storage configuration, or nil.
	Storage *LegacyStorage
	// MongoURI and MongoURISource are the former default MongoDB connection string.
	MongoURI       string
	MongoURISource string
	// APIKey and APIKeySource are the former static API key.
	APIKey       string
	APIKeySource string
	// DBPath is the former database path override (no longer supported).
	DBPath string
	// SecretKey is a secret_key found in the legacy file (bootstrap only).
	SecretKey string
	// File is the legacy configuration file that was read, or "".
	File string
	// Present lists every deprecated source that is set.
	Present []string
	// Problems lists sources that could not be read; they are skipped.
	Problems []string
}

// LegacyStorage is a storage target described by deprecated sources.
type LegacyStorage struct {
	// Target holds the type and the local or S3 configuration.
	Target models.StorageTarget
	// Sources lists the sources it was built from.
	Sources []string
}

// Source returns a single source name for the storage import marker.
func (l *LegacyStorage) Source() string {
	return "storage:" + strings.Join(l.Sources, ",")
}

// legacyFile is the part of the former JSON configuration that is imported. Pointer
// fields tell absent keys from zero values.
type legacyFile struct {
	Server struct {
		APIKey            *string  `json:"api_key"`
		TrustProxyHeaders *bool    `json:"trust_proxy_headers"`
		SecureCookies     *bool    `json:"secure_cookies"`
		CORSOrigins       []string `json:"cors_origins"`
		MetricsPublic     *bool    `json:"metrics_public"`
	} `json:"server"`
	Mongo struct {
		URI *string `json:"uri"`
	} `json:"mongo"`
	Storage struct {
		Type  *string `json:"type"`
		Local struct {
			Path *string `json:"path"`
		} `json:"local"`
		S3 struct {
			Endpoint     *string `json:"endpoint"`
			Bucket       *string `json:"bucket"`
			Region       *string `json:"region"`
			AccessKey    *string `json:"access_key"`
			SecretKey    *string `json:"secret_key"`
			UsePathStyle *bool   `json:"use_path_style"`
		} `json:"s3"`
	} `json:"storage"`
	Defaults struct {
		RetentionDays  *int  `json:"retention_days"`
		RetentionCount *int  `json:"retention_count"`
		Gzip           *bool `json:"gzip"`
	} `json:"defaults"`
	Encryption struct {
		Enabled      *bool    `json:"enabled"`
		Recipients   []string `json:"recipients"`
		Identity     *string  `json:"identity"`
		IdentityFile *string  `json:"identity_file"`
		Passphrase   *string  `json:"passphrase"`
	} `json:"encryption"`
	Backup struct {
		Timeout      *int64 `json:"timeout"`
		StallTimeout *int64 `json:"stall_timeout"`
	} `json:"backup"`
	Restore struct {
		Timeout             *int64  `json:"timeout"`
		VerifyBeforeRestore *string `json:"verify_before_restore"`
	} `json:"restore"`
	DatabasePath *string `json:"database_path"`
	SecretKey    *string `json:"secret_key"`
}

// legacyBuilder collects imports; environment variables override the file.
type legacyBuilder struct {
	out      *Legacy
	settings map[string]settings.Import
	storage  map[string]string // field -> value
	sources  map[string]string // storage field -> source
}

// LoadLegacy reads the deprecated environment variables and <dataDir>/config.json.
// Values that cannot be interpreted are reported in Problems and skipped; only a
// config.json that exists but cannot be parsed is an error.
func LoadLegacy(dataDir string, getenv func(string) string) (*Legacy, error) {
	b := &legacyBuilder{out: &Legacy{}, settings: map[string]settings.Import{}, storage: map[string]string{}, sources: map[string]string{}}
	if err := b.readFile(filepath.Join(dataDir, LegacyFileName)); err != nil {
		return nil, err
	}
	b.readEnv(getenv)
	return b.finish(), nil
}

func (b *legacyBuilder) setting(key string, value any, source string) {
	b.settings[key] = settings.Import{Key: key, Value: value, Source: source}
	b.present(source)
}

func (b *legacyBuilder) storageField(field, value, source string) {
	b.storage[field], b.sources[field] = value, source
	if source != envAWSRegion && source != envAWSAccessKey && source != envAWSSecretKey {
		b.present(source)
	}
}

func (b *legacyBuilder) present(source string) {
	for _, p := range b.out.Present {
		if p == source {
			return
		}
	}
	b.out.Present = append(b.out.Present, source)
}

func (b *legacyBuilder) problem(source string, err error) {
	b.out.Problems = append(b.out.Problems, fmt.Sprintf("%s: %v", source, err))
}

// readFile imports <dataDir>/config.json when it exists.
func (b *legacyBuilder) readFile(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("config: stat legacy %s: %w", path, err)
	case info.Size() > maxLegacyFileSize:
		return fmt.Errorf("config: legacy %s is larger than %d bytes", path, maxLegacyFileSize)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the legacy file inside the configured data directory.
	if err != nil {
		return fmt.Errorf("config: read legacy %s: %w", path, err)
	}
	var f legacyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("config: parse legacy %s: %w", path, err)
	}
	b.out.File = path
	src := func(key string) string { return LegacyFileName + ":" + key }

	if v := f.Server.APIKey; v != nil && *v != "" {
		b.out.APIKey, b.out.APIKeySource = *v, src("server.api_key")
		b.present(b.out.APIKeySource)
	}
	if v := f.Server.TrustProxyHeaders; v != nil {
		b.setting(settings.KeyTrustProxyHeaders, *v, src("server.trust_proxy_headers"))
	}
	if v := f.Server.SecureCookies; v != nil {
		b.setting(settings.KeySecureCookies, cookiePolicy(*v), src("server.secure_cookies"))
	}
	if f.Server.CORSOrigins != nil {
		b.setting(settings.KeyCORSOrigins, f.Server.CORSOrigins, src("server.cors_origins"))
	}
	if v := f.Server.MetricsPublic; v != nil {
		b.setting(settings.KeyMetricsPublic, *v, src("server.metrics_public"))
	}
	if v := f.Mongo.URI; v != nil && *v != "" {
		b.out.MongoURI, b.out.MongoURISource = *v, src("mongo.uri")
		b.present(b.out.MongoURISource)
	}
	st := f.Storage
	for field, v := range map[string]*string{
		"type": st.Type, "local.path": st.Local.Path, "s3.endpoint": st.S3.Endpoint, "s3.bucket": st.S3.Bucket,
		"s3.region": st.S3.Region, "s3.access_key": st.S3.AccessKey, "s3.secret_key": st.S3.SecretKey,
	} {
		if v != nil && *v != "" {
			b.storageField(field, *v, src("storage."+field))
		}
	}
	if v := st.S3.UsePathStyle; v != nil && *v {
		b.storageField("s3.use_path_style", "true", src("storage.s3.use_path_style"))
	}
	if v := f.Defaults.RetentionDays; v != nil {
		b.setting(settings.KeyDefaultRetentionDays, *v, src("defaults.retention_days"))
	}
	if v := f.Defaults.RetentionCount; v != nil {
		b.setting(settings.KeyDefaultRetentionCount, *v, src("defaults.retention_count"))
	}
	if v := f.Defaults.Gzip; v != nil {
		b.setting(settings.KeyDefaultGzip, *v, src("defaults.gzip"))
	}
	for key, pair := range map[string]struct {
		v    *int64
		name string
	}{
		settings.KeyBackupTimeout:      {f.Backup.Timeout, "backup.timeout"},
		settings.KeyBackupStallTimeout: {f.Backup.StallTimeout, "backup.stall_timeout"},
		settings.KeyRestoreTimeout:     {f.Restore.Timeout, "restore.timeout"},
	} {
		if pair.v != nil {
			b.setting(key, time.Duration(*pair.v).String(), src(pair.name))
		}
	}
	if v := f.Restore.VerifyBeforeRestore; v != nil && *v != "" {
		b.setting(settings.KeyRestoreVerifyPolicy, strings.ToLower(*v), src("restore.verify_before_restore"))
	}
	enc := f.Encryption
	if enc.Recipients != nil {
		b.setting(settings.KeyEncryptionRecipients, enc.Recipients, src("encryption.recipients"))
	}
	identity := ""
	if v := enc.Identity; v != nil {
		identity = *v
	}
	if v := enc.IdentityFile; v != nil && *v != "" {
		if content, err := readIdentityFile(*v); err != nil {
			b.problem(src("encryption.identity_file"), err)
		} else {
			identity = strings.TrimSpace(identity + "\n" + content)
		}
	}
	if identity != "" {
		b.setting(settings.KeyEncryptionIdentity, identity, src("encryption.identity"))
	}
	if v := enc.Passphrase; v != nil && *v != "" {
		b.setting(settings.KeyEncryptionPassphrase, *v, src("encryption.passphrase"))
	}
	if v := enc.Enabled; v != nil {
		b.setting(settings.KeyEncryptionEnabled, *v, src("encryption.enabled"))
	}
	if v := f.DatabasePath; v != nil && *v != "" {
		b.out.DBPath = *v
		b.present(src("database_path"))
	}
	if v := f.SecretKey; v != nil && *v != "" {
		b.out.SecretKey = *v
	}
	return nil
}

// readEnv imports the deprecated environment variables (they override the file).
func (b *legacyBuilder) readEnv(getenv func(string) string) {
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	boolEnv := func(name string, key string, conv func(bool) any) {
		v := get(name)
		if v == "" {
			return
		}
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			b.present(name)
			b.problem(name, errors.New("not a boolean"))
			return
		}
		b.setting(key, conv(parsed), name)
	}
	same := func(v bool) any { return v }

	if v := get(EnvAPIKey); v != "" {
		b.out.APIKey, b.out.APIKeySource = v, EnvAPIKey
		b.present(EnvAPIKey)
	}
	if v := get(EnvMongoURI); v != "" {
		b.out.MongoURI, b.out.MongoURISource = v, EnvMongoURI
		b.present(EnvMongoURI)
	}
	if v := get(EnvDBPath); v != "" {
		b.out.DBPath = filepath.Clean(v)
		b.present(EnvDBPath)
	}
	if v := get(EnvCORSOrigins); v != "" {
		b.setting(settings.KeyCORSOrigins, parseCSV(v), EnvCORSOrigins)
	}
	boolEnv(EnvMetricsPublic, settings.KeyMetricsPublic, same)
	boolEnv(EnvTrustProxy, settings.KeyTrustProxyHeaders, same)
	boolEnv(EnvSecureCookies, settings.KeySecureCookies, func(v bool) any { return cookiePolicy(v) })
	for name, key := range map[string]string{
		EnvBackupTimeout:  settings.KeyBackupTimeout,
		EnvBackupStall:    settings.KeyBackupStallTimeout,
		EnvRestoreTimeout: settings.KeyRestoreTimeout,
	} {
		if v := get(name); v != "" {
			b.setting(key, v, name)
		}
	}
	if v := get(EnvRestoreVerify); v != "" {
		b.setting(settings.KeyRestoreVerifyPolicy, strings.ToLower(v), EnvRestoreVerify)
	}

	for field, name := range map[string]string{
		"type": EnvStorageType, "local.path": EnvLocalPath, "s3.bucket": EnvS3Bucket, "s3.endpoint": EnvS3Endpoint,
	} {
		if v := get(name); v != "" {
			b.storageField(field, v, name)
		}
	}
	if v := get(EnvS3PathStyle); v != "" {
		b.storageField("s3.use_path_style", strconv.FormatBool(strings.EqualFold(v, "true") || v == "1"), EnvS3PathStyle)
	}
	for field, name := range map[string]string{"s3.region": envAWSRegion, "s3.access_key": envAWSAccessKey, "s3.secret_key": envAWSSecretKey} {
		if v := get(name); v != "" {
			b.storageField(field, v, name)
		}
	}

	if v := get(EnvEncryptionRecips); v != "" {
		b.setting(settings.KeyEncryptionRecipients, parseCSV(v), EnvEncryptionRecips)
	}
	identity, source := getenv(EnvEncryptionIdentity), EnvEncryptionIdentity
	if path := get(EnvEncryptionIDFile); path != "" {
		b.present(EnvEncryptionIDFile)
		if content, err := readIdentityFile(path); err != nil {
			b.problem(EnvEncryptionIDFile, err)
		} else {
			identity = strings.TrimSpace(identity + "\n" + content)
			if strings.TrimSpace(getenv(EnvEncryptionIdentity)) == "" {
				source = EnvEncryptionIDFile
			}
		}
	}
	if strings.TrimSpace(identity) != "" {
		b.setting(settings.KeyEncryptionIdentity, strings.TrimSpace(identity), source)
	}
	if v := getenv(EnvEncryptionPass); v != "" {
		b.setting(settings.KeyEncryptionPassphrase, v, EnvEncryptionPass)
	}
	boolEnv(EnvEncryptionEnabled, settings.KeyEncryptionEnabled, same)
}

// finish orders the setting imports (keys before the switches that need them) and
// builds the storage target.
func (b *legacyBuilder) finish() *Legacy {
	recipients, hasRecipients := b.settings[settings.KeyEncryptionRecipients]
	_, hasPassphrase := b.settings[settings.KeyEncryptionPassphrase]
	if enabled, ok := b.settings[settings.KeyEncryptionEnabled]; ok {
		mode := settings.ModeX25519
		if list, _ := recipients.Value.([]string); (!hasRecipients || len(list) == 0) && hasPassphrase {
			mode = settings.ModePassphrase
		}
		b.settings[settings.KeyEncryptionMode] = settings.Import{Key: settings.KeyEncryptionMode, Value: mode, Source: enabled.Source}
	}
	order := append(settings.Keys(), "")
	// Import the enable switch last: it is only valid once keys are present.
	order = moveToEnd(order, settings.KeyEncryptionEnabled)
	for _, key := range order {
		if it, ok := b.settings[key]; ok {
			b.out.Settings = append(b.out.Settings, it)
		}
	}
	b.out.Storage = b.buildStorage()
	return b.out
}

func moveToEnd(list []string, key string) []string {
	out := make([]string, 0, len(list))
	for _, k := range list {
		if k != key {
			out = append(out, k)
		}
	}
	return append(out, key)
}

// buildStorage turns the collected storage fields into a target, or nil when only
// standard AWS variables (not a storage configuration) are present.
func (b *legacyBuilder) buildStorage() *LegacyStorage {
	typ := strings.ToLower(b.storage["type"])
	if typ == "" {
		switch {
		case b.storage["s3.bucket"] != "":
			typ = string(models.StorageS3)
		case b.storage["local.path"] != "":
			typ = string(models.StorageLocal)
		default:
			return nil
		}
	}
	ls := &LegacyStorage{}
	use := func(fields ...string) {
		for _, f := range fields {
			if src, ok := b.sources[f]; ok {
				ls.Sources = append(ls.Sources, src)
			}
		}
	}
	switch models.StorageType(typ) {
	case models.StorageLocal:
		path := b.storage["local.path"]
		if path == "" {
			path = "./" + DefaultBackupsDirName
		}
		ls.Target = models.StorageTarget{Type: models.StorageLocal, Local: &models.LocalTarget{Path: path}}
		use("type", "local.path")
	case models.StorageS3:
		ls.Target = models.StorageTarget{Type: models.StorageS3, S3: &models.S3Target{
			Endpoint:        b.storage["s3.endpoint"],
			Region:          b.storage["s3.region"],
			Bucket:          b.storage["s3.bucket"],
			AccessKeyID:     b.storage["s3.access_key"],
			SecretAccessKey: b.storage["s3.secret_key"],
			UsePathStyle:    b.storage["s3.use_path_style"] == "true",
		}}
		use("type", "s3.bucket", "s3.endpoint", "s3.region", "s3.access_key", "s3.secret_key", "s3.use_path_style")
	default:
		b.problem(b.sources["type"], fmt.Errorf("unknown storage type %q", typ))
		return nil
	}
	return ls
}

// cookiePolicy maps the former secure_cookies switch to a policy.
func cookiePolicy(always bool) settings.CookiePolicy {
	if always {
		return settings.CookiesAlways
	}
	return settings.CookiesAuto
}

// readIdentityFile reads an age identity file of a former configuration.
func readIdentityFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read identity file: %w", err)
	}
	if info.Size() > maxLegacyFileSize {
		return "", errors.New("identity file is too large")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: an operator-configured identity file of an earlier release.
	if err != nil {
		return "", fmt.Errorf("read identity file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// parseCSV splits a comma-separated list, trimming whitespace and dropping empty entries.
func parseCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DeprecatedEnvSet returns the deprecated variables that are set in the environment.
func DeprecatedEnvSet(getenv func(string) string) []string {
	var out []string
	for _, name := range deprecatedEnv {
		if strings.TrimSpace(getenv(name)) != "" {
			out = append(out, name)
		}
	}
	return out
}
