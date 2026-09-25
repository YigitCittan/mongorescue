// Package config holds the bootstrap options MongoRescue needs before it can open its
// database: the data directory, the listen address, the log level and the optional
// secret key. Everything else (storage targets, encryption, security, limits) is
// managed in the dashboard and stored in the database (internal/settings).
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
}

// Default returns the defaults of the binary.
func Default() *Config {
	return &Config{DataDir: DefaultDataDir, Host: DefaultHost, Port: DefaultPort, LogLevel: slog.LevelInfo}
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
	return cfg, nil
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
	}
	if c.SecretKey != "" {
		if _, err := secretbox.ParseKey(c.SecretKey); err != nil {
			// The error never contains the key.
			return fmt.Errorf("%s: %w", EnvSecretKey, err)
		}
	}
	return nil
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
