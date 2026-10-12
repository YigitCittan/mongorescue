// Package config holds the bootstrap options MongoRescue needs before it can open its
// database: the data directory, the listen address, the log level, the optional
// secret key, the MongoDB Database Tools directory and whether the web dashboard is
// served. Everything else (storage
// targets, encryption, security, limits) is managed in the dashboard and stored in
// the database (internal/settings).
//
// The package also reads the environment variables and the <data_dir>/config.json
// file of earlier releases (see LoadLegacy), so they can be imported into the database
// once.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Bootstrap environment variables. They are the only environment variables
// MongoRescue reads (flags take precedence).
const (
	// EnvDataDir sets the data directory.
	EnvDataDir = "MONGORESCUE_DATA_DIR"
	// EnvHost sets the listen address.
	EnvHost = "MONGORESCUE_SERVER_HOST"
	// EnvPort sets the listen port.
	EnvPort = "MONGORESCUE_SERVER_PORT"
	// EnvSecretKey supplies the base64 key that encrypts stored credentials instead of
	// <data_dir>/secret.key.
	EnvSecretKey = "MONGORESCUE_SECRET_KEY" //nolint:gosec // G101: a variable name, not a credential.
	// EnvDashboard enables the embedded web dashboard on the HTTP server ("true",
	// "1", ...; see strconv.ParseBool). The container image sets it; installer-based
	// installs use the desktop app instead.
	EnvDashboard = "MONGORESCUE_DASHBOARD"
	// EnvToolsDir sets a directory searched first for mongodump and mongorestore
	// (before <executable dir>/tools and PATH; see mongotools.Resolver).
	EnvToolsDir = mongotools.EnvToolsDir
	// EnvShutdownGrace sets how long a shutdown (SIGTERM) waits for running backups
	// and restores to finish before it cancels them: a Go duration such as "9m" or a
	// number of seconds.
	EnvShutdownGrace = "MONGORESCUE_SHUTDOWN_GRACE"
	// EnvTmpDir sets the directory of the short-lived files that pass connection
	// strings and TLS material to mongodump and mongorestore. Empty uses the
	// system temporary directory (TMPDIR), or <data_dir>/tmp when that is not
	// writable.
	EnvTmpDir = "MONGORESCUE_TMP_DIR"
	// EnvMinFreeSpace sets the free space, in MiB, the data directory needs for a
	// backup or restore to start (0 turns the check off).
	EnvMinFreeSpace = "MONGORESCUE_MIN_FREE_SPACE_MB"
	// EnvNotificationMaxAge sets how long a notification may wait in the delivery
	// queue before it is dropped as stale: a Go duration such as "24h".
	EnvNotificationMaxAge = "MONGORESCUE_NOTIFICATION_MAX_AGE"
)

// Defaults.
const (
	// DefaultDataDir is the data directory of the binary (the container image sets /data).
	DefaultDataDir = "./data"
	// DefaultHost binds every interface.
	DefaultHost = "0.0.0.0"
	// DefaultPort is the HTTP port.
	DefaultPort = 8080
	// DefaultDatabaseFileName is the metadata database file inside the data directory.
	DefaultDatabaseFileName = "mongorescue.db"
	// DefaultBackupsDirName is the directory of the default "Local disk" storage
	// target, next to the data directory (./backups, or /backups in the image).
	DefaultBackupsDirName = "backups"
	// MaxShutdownGrace is the longest shutdown grace period.
	MaxShutdownGrace = 24 * time.Hour
	// DefaultMinFreeSpaceMB is the free space (MiB) the data directory needs for a
	// backup or restore to start.
	DefaultMinFreeSpaceMB = 100
	// MaxMinFreeSpaceMB is the largest minimum free space (1 TiB).
	MaxMinFreeSpaceMB = 1 << 20
	// DefaultNotificationMaxAge is how long a notification may wait to be sent.
	DefaultNotificationMaxAge = 24 * time.Hour
	// MinNotificationMaxAge and MaxNotificationMaxAge bound it.
	MinNotificationMaxAge = 10 * time.Minute
	MaxNotificationMaxAge = 30 * 24 * time.Hour
)

// Sentinel errors.
var (
	// ErrInvalidPort is returned for a port outside 1-65535.
	ErrInvalidPort = errors.New("config: port must be between 1 and 65535")
	// ErrInvalidDataDir is returned for an empty data directory.
	ErrInvalidDataDir = errors.New("config: data directory must not be empty")
	// ErrInvalidHost is returned for a host containing whitespace.
	ErrInvalidHost = errors.New("config: host must not contain whitespace")
	// ErrInvalidLogLevel is returned for an unknown log level.
	ErrInvalidLogLevel = errors.New("config: log level must be debug, info, warn or error")
	// ErrInvalidDashboard is returned for a MONGORESCUE_DASHBOARD value that is not a
	// boolean.
	ErrInvalidDashboard = errors.New("config: dashboard must be a boolean (true or false)")
	// ErrInvalidToolsDir is returned for a tools directory that is not an absolute
	// path (a relative one would let the working directory supply the binaries).
	ErrInvalidToolsDir = errors.New("config: tools directory must be an absolute path")
	// ErrInvalidTmpDir is returned for a temporary directory that is not an
	// absolute path.
	ErrInvalidTmpDir = errors.New("config: temporary directory must be an absolute path")
	// ErrInvalidShutdownGrace is returned for a shutdown grace period that is not a
	// duration from 0 to MaxShutdownGrace.
	ErrInvalidShutdownGrace = errors.New("config: shutdown grace must be a duration from 0s to 24h (such as 9m) or a number of seconds")
	// ErrInvalidMinFreeSpace is returned for a minimum free space that is not a
	// whole number of MiB from 0 to MaxMinFreeSpaceMB.
	ErrInvalidMinFreeSpace = errors.New("config: minimum free space must be a whole number of MiB from 0 to 1048576")
	// ErrInvalidNotificationMaxAge is returned for a notification maximum age that
	// is not a duration from 10m to 720h.
	ErrInvalidNotificationMaxAge = errors.New("config: notification max age must be a duration from 10m to 720h (such as 24h)")
)

// Config is the bootstrap configuration.
type Config struct {
	// DataDir holds mongorescue.db, secret.key and the data directory lock.
	DataDir string
	// Host is the listen address.
	Host string
	// Port is the listen port.
	Port int
	// LogLevel is the minimum level logged.
	LogLevel slog.Level
	// SecretKey is the optional base64 key (MONGORESCUE_SECRET_KEY) that encrypts
	// stored credentials. Empty means <DataDir>/secret.key, generated on first start.
	SecretKey string
	// Dashboard serves the embedded web dashboard at "/". Off by default: the REST
	// API, MCP and metrics work without it.
	Dashboard bool
	// ToolsDir is the absolute directory searched first for mongodump and
	// mongorestore. Empty means the bundled locations next to the executable, then PATH.
	ToolsDir string
	// ShutdownGrace is how long a shutdown waits for running backups and restores to
	// finish (new ones are refused meanwhile) before it cancels them. Zero cancels
	// them at once. Keep it below the time the process manager allows for the stop
	// (Kubernetes terminationGracePeriodSeconds, docker stop -t), leaving about a
	// minute for the cancellation.
	ShutdownGrace time.Duration
	// TmpDir is the absolute directory of the short-lived files that pass
	// connection strings and TLS material to the Database Tools. Empty means the
	// system temporary directory, or <DataDir>/tmp when that is not writable.
	TmpDir string
	// MinFreeSpaceMB is the free space (MiB) the data directory needs for a backup
	// or restore to start; 0 turns the check off. A metadata write that fails
	// because the disk is full stops new runs whatever the value, until space is
	// freed (see internal/diskguard).
	MinFreeSpaceMB int
	// NotificationMaxAge is how long a notification may wait in the delivery
	// queue (behind older ones of its channel, or across a downtime) before it is
	// dropped as stale.
	NotificationMaxAge time.Duration
}

// Default returns the defaults of the binary.
func Default() *Config {
	return &Config{DataDir: DefaultDataDir, Host: DefaultHost, Port: DefaultPort, LogLevel: slog.LevelInfo,
		MinFreeSpaceMB: DefaultMinFreeSpaceMB, NotificationMaxAge: DefaultNotificationMaxAge}
}

// FromEnv returns the defaults overridden by the bootstrap environment variables.
// getenv is usually os.Getenv.
func FromEnv(getenv func(string) string) (*Config, error) {
	cfg := Default()
	if v := strings.TrimSpace(getenv(EnvDataDir)); v != "" {
		cfg.DataDir = filepath.Clean(v)
	}
	if v := strings.TrimSpace(getenv(EnvHost)); v != "" {
		cfg.Host = v
	}
	if v := strings.TrimSpace(getenv(EnvPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvPort, ErrInvalidPort)
		}
		cfg.Port = p
	}
	if v := strings.TrimSpace(getenv(EnvSecretKey)); v != "" {
		cfg.SecretKey = v
	}
	if v := strings.TrimSpace(getenv(EnvToolsDir)); v != "" {
		cfg.ToolsDir = filepath.Clean(v)
	}
	if v := strings.TrimSpace(getenv(EnvTmpDir)); v != "" {
		cfg.TmpDir = filepath.Clean(v)
	}
	if v := strings.TrimSpace(getenv(EnvDashboard)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvDashboard, ErrInvalidDashboard)
		}
		cfg.Dashboard = b
	}
	if v := strings.TrimSpace(getenv(EnvShutdownGrace)); v != "" {
		d, err := ParseShutdownGrace(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvShutdownGrace, err)
		}
		cfg.ShutdownGrace = d
	}
	if v := strings.TrimSpace(getenv(EnvMinFreeSpace)); v != "" {
		n, err := ParseMinFreeSpace(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvMinFreeSpace, err)
		}
		cfg.MinFreeSpaceMB = n
	}
	if v := strings.TrimSpace(getenv(EnvNotificationMaxAge)); v != "" {
		d, err := ParseNotificationMaxAge(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvNotificationMaxAge, err)
		}
		cfg.NotificationMaxAge = d
	}
	return cfg, nil
}

// ParseNotificationMaxAge parses a notification maximum age: a Go duration from
// MinNotificationMaxAge to MaxNotificationMaxAge.
func ParseNotificationMaxAge(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < MinNotificationMaxAge || d > MaxNotificationMaxAge {
		return 0, ErrInvalidNotificationMaxAge
	}
	return d, nil
}

// ParseMinFreeSpace parses a minimum free space: a whole number of MiB from 0 to
// MaxMinFreeSpaceMB.
func ParseMinFreeSpace(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 || n > MaxMinFreeSpaceMB {
		return 0, ErrInvalidMinFreeSpace
	}
	return n, nil
}

// ParseShutdownGrace parses a shutdown grace period: a Go duration ("9m", "540s")
// or a whole number of seconds ("540"), from 0 to MaxShutdownGrace.
func ParseShutdownGrace(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	d, err := time.ParseDuration(v)
	if err != nil {
		n, convErr := strconv.Atoi(v)
		if convErr != nil {
			return 0, ErrInvalidShutdownGrace
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 || d > MaxShutdownGrace {
		return 0, ErrInvalidShutdownGrace
	}
	return d, nil
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	switch {
	case strings.TrimSpace(c.DataDir) == "":
		return ErrInvalidDataDir
	case c.Port < 1 || c.Port > 65535:
		return ErrInvalidPort
	case strings.ContainsAny(c.Host, " \t\r\n"):
		return ErrInvalidHost
	case c.ToolsDir != "" && !filepath.IsAbs(c.ToolsDir):
		return fmt.Errorf("%s: %w", EnvToolsDir, ErrInvalidToolsDir)
	case c.TmpDir != "" && !filepath.IsAbs(c.TmpDir):
		return fmt.Errorf("%s: %w", EnvTmpDir, ErrInvalidTmpDir)
	case c.ShutdownGrace < 0 || c.ShutdownGrace > MaxShutdownGrace:
		return fmt.Errorf("%s: %w", EnvShutdownGrace, ErrInvalidShutdownGrace)
	case c.MinFreeSpaceMB < 0 || c.MinFreeSpaceMB > MaxMinFreeSpaceMB:
		return fmt.Errorf("%s: %w", EnvMinFreeSpace, ErrInvalidMinFreeSpace)
	case c.NotificationMaxAge != 0 && (c.NotificationMaxAge < MinNotificationMaxAge || c.NotificationMaxAge > MaxNotificationMaxAge):
		return fmt.Errorf("%s: %w", EnvNotificationMaxAge, ErrInvalidNotificationMaxAge)
	}
	if c.SecretKey != "" {
		if _, err := secretbox.ParseKey(c.SecretKey); err != nil {
			// The error never contains the key.
			return fmt.Errorf("%s: %w", EnvSecretKey, err)
		}
	}
	return nil
}

// MinFreeSpaceBytes returns MinFreeSpaceMB in bytes (0 when the check is off).
func (c *Config) MinFreeSpaceBytes() uint64 {
	if c.MinFreeSpaceMB <= 0 {
		return 0
	}
	return uint64(min(c.MinFreeSpaceMB, MaxMinFreeSpaceMB)) << 20 //nolint:gosec // G115: positive and at most MaxMinFreeSpaceMB.
}

// MetadataDBPath returns the metadata database file inside the data directory.
func (c *Config) MetadataDBPath() string {
	return filepath.Join(c.DataDir, DefaultDatabaseFileName)
}

// BaseDir returns the absolute parent of the data directory. Relative paths of local
// storage targets are resolved against it.
func (c *Config) BaseDir() (string, error) {
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return "", fmt.Errorf("config: resolve data directory: %w", err)
	}
	return filepath.Dir(abs), nil
}

// DefaultBackupsDir returns the directory of the default "Local disk" storage target:
// DefaultBackupsDirName next to the data directory (/backups in the container image).
func (c *Config) DefaultBackupsDir() (string, error) {
	base, err := c.BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DefaultBackupsDirName), nil
}

// ParseLogLevel parses debug, info, warn or error (case-insensitive).
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, ErrInvalidLogLevel
}
