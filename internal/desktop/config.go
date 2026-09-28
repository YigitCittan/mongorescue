package desktop

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/config"
)

// AppDirName is the per-user application directory of the desktop app inside
// os.UserConfigDir (for example ~/Library/Application Support/MongoRescue).
const AppDirName = "MongoRescue"

// dataDirName is the data directory inside AppDirName. The default "Local disk"
// storage target lands next to it, in AppDirName/backups.
const dataDirName = "data"

// ErrNoUserConfigDir is returned when the per-user configuration directory is unknown
// and no data directory was given.
var ErrNoUserConfigDir = errors.New("desktop: cannot determine the user configuration directory; pass -data-dir")

// DefaultDataDir returns the data directory of the desktop app:
// os.UserConfigDir()/MongoRescue/data.
func DefaultDataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoUserConfigDir, err)
	}
	if dir == "" {
		return "", ErrNoUserConfigDir
	}
	return filepath.Join(dir, AppDirName, dataDirName), nil
}

// ParseConfig returns the bootstrap configuration of the desktop app: the dashboard
// is always on, the data directory is -data-dir, else MONGORESCUE_DATA_DIR, else
// DefaultDataDir, and MONGORESCUE_SECRET_KEY is honoured. The listen address and
// MONGORESCUE_DASHBOARD are not read, since the desktop app never opens a listener
// and always shows the dashboard. -log-level sets the log level.
func ParseConfig(args []string, getenv func(string) string, stderr io.Writer) (*config.Config, error) {
	fs := flag.NewFlagSet("mongorescue-desktop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "Data directory holding mongorescue.db and secret.key (env "+config.EnvDataDir+", default <user config dir>/"+AppDirName+"/"+dataDirName+")")
	logLevel := fs.String("log-level", "info", "Log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		return nil, err
	}
	// Only the variables that apply to the desktop app are read: a leftover
	// MONGORESCUE_SERVER_PORT or MONGORESCUE_DASHBOARD must not keep it from starting.
	cfg := config.Default()
	switch env := strings.TrimSpace(getenv(config.EnvDataDir)); {
	case *dataDir != "":
		cfg.DataDir = filepath.Clean(*dataDir)
	case env != "":
		cfg.DataDir = filepath.Clean(env)
	default:
		if cfg.DataDir, err = DefaultDataDir(); err != nil {
			return nil, err
		}
	}
	cfg.SecretKey = strings.TrimSpace(getenv(config.EnvSecretKey))
	cfg.LogLevel = level
	cfg.Dashboard = true
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NewLogger returns a text logger writing to <DataDir>/desktop.log (truncated on
// every start) and to stderr, and the log file to close on exit. The data directory
// is created with mode 0700 when missing.
func NewLogger(cfg *config.Config, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("desktop: create data directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(cfg.DataDir, "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("desktop: open log file: %w", err)
	}
	// The file comes first: a GUI process on Windows has no usable stderr, and
	// io.MultiWriter stops at the first failing writer.
	w := io.MultiWriter(f, stderr)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel})), f, nil
}
