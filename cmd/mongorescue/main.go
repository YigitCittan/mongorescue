// Package main is the entrypoint for MongoRescue CLI and embedded service.
//
// Only bootstrap options are read here (data directory, listen address, log level,
// the optional secret key, the MongoDB Database Tools directory and whether the web
// dashboard is served); everything else
// is configured in the dashboard and stored in the database. "mongorescue mcp" runs
// the stdio bridge for AI assistants instead of the server, and "mongorescue backup",
// "restore", "list", "verify" and "status" run the CLI (internal/cli), a client of a
// running server's REST API.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/cli"
	"github.com/yigitcittan/mongorescue/internal/config"
)

// Version metadata populated at build time via -ldflags.
var (
	Version = "1.0.0"
	Commit  = "dev"
	Date    = "unknown"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// dispatch runs the command args name: "mcp" (the stdio bridge), a CLI command
// (backup, restore, list, verify, status, help) or, without one, the server. It
// returns the process exit code.
func dispatch(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch {
		case args[0] == mcpCommand:
			return runMCP(args[1:], getenv, stdin, stdout, stderr)
		case cli.IsCommand(args[0]):
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return (&cli.App{Version: Version}).Run(ctx, args, getenv, stdout, stderr)
		}
	}
	return run(args, getenv, stdout, stderr)
}

// run parses flags, builds the logger and runs the application. It returns the
// process exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, logLevel, showVersion, err := parseFlags(args, getenv, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if showVersion {
		fmt.Fprintf(stdout, "MongoRescue v%s (commit: %s, built: %s)\n", Version, Commit, Date)
		return 0
	}
	cfg.LogLevel = logLevel

	logger := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	logger.Info("booting mongorescue disaster recovery engine",
		slog.String("version", Version),
		slog.String("data_dir", cfg.DataDir),
	)

	application, err := app.New(cfg, logger, app.WithBuildInfo(Version, Commit), app.WithGetenv(getenv))
	if err != nil {
		logger.Error("application bootstrap failed", slog.Any("error", err))
		return 1
	}
	if err := application.Run(); err != nil {
		logger.Error("application execution terminated with error", slog.Any("error", err))
		return 1
	}
	logger.Info("mongorescue shutdown complete. Goodbye!")
	return 0
}

// parseFlags reads the bootstrap environment variables and overrides them with the
// flags that are set.
func parseFlags(args []string, getenv func(string) string, stderr io.Writer) (*config.Config, slog.Level, bool, error) {
	fs := flag.NewFlagSet("mongorescue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "Data directory holding mongorescue.db and secret.key (env "+config.EnvDataDir+", default "+config.DefaultDataDir+")")
	host := fs.String("host", "", "Listen address (env "+config.EnvHost+", default "+config.DefaultHost+")")
	port := fs.Int("port", 0, fmt.Sprintf("Listen port (env %s, default %d)", config.EnvPort, config.DefaultPort))
	dashboard := fs.Bool("dashboard", false, "Serve the embedded web dashboard (env "+config.EnvDashboard+", default false)")
	toolsDir := fs.String("tools-dir", "", "Directory searched first for mongodump and mongorestore (env "+config.EnvToolsDir+", default <executable dir>/tools, then PATH)")
	tmpDir := fs.String("tmp-dir", "", "Directory of the short-lived files that pass connection strings and TLS material to mongodump and mongorestore (env "+config.EnvTmpDir+", default the system temporary directory, or <data-dir>/tmp when it is not writable)")
	shutdownGrace := fs.String("shutdown-grace", "", "How long a shutdown waits for running backups and restores before it cancels them, e.g. 9m (env "+config.EnvShutdownGrace+", default 0s: cancel at once)")
	minFreeSpace := fs.String("min-free-space-mb", "", fmt.Sprintf("Free space (MiB) the data directory needs for a backup or restore to start, 0 to turn the check off (env %s, default %d)", config.EnvMinFreeSpace, config.DefaultMinFreeSpaceMB))
	notifyMaxAge := fs.String("notification-max-age", "", "How long a notification may wait to be sent before it is dropped as stale, e.g. 24h (env "+config.EnvNotificationMaxAge+", default 24h)")
	logLevel := fs.String("log-level", "info", "Log level: debug, info, warn or error")
	showVersion := fs.Bool("version", false, "Print version information and exit")
	if err := fs.Parse(args); err != nil {
		return nil, 0, false, err
	}
	if *showVersion {
		return config.Default(), slog.LevelInfo, true, nil
	}
	if fs.NArg() > 0 {
		return nil, 0, false, fmt.Errorf("unexpected arguments: %v (everything except the bootstrap flags is configured in the dashboard; CLI commands: backup, restore, list, verify, status, help)", fs.Args())
	}
	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		return nil, 0, false, err
	}
	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return nil, 0, false, err
	}
	var graceErr, spaceErr, ageErr error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "data-dir":
			cfg.DataDir = filepath.Clean(*dataDir)
		case "host":
			cfg.Host = *host
		case "port":
			cfg.Port = *port
		case "dashboard":
			cfg.Dashboard = *dashboard
		case "tools-dir":
			// An empty flag clears the directory; Clean would turn it into ".".
			cfg.ToolsDir = ""
			if v := strings.TrimSpace(*toolsDir); v != "" {
				cfg.ToolsDir = filepath.Clean(v)
			}
		case "tmp-dir":
			cfg.TmpDir = ""
			if v := strings.TrimSpace(*tmpDir); v != "" {
				cfg.TmpDir = filepath.Clean(v)
			}
		case "shutdown-grace":
			cfg.ShutdownGrace, graceErr = config.ParseShutdownGrace(*shutdownGrace)
		case "min-free-space-mb":
			cfg.MinFreeSpaceMB, spaceErr = config.ParseMinFreeSpace(*minFreeSpace)
		case "notification-max-age":
			cfg.NotificationMaxAge, ageErr = config.ParseNotificationMaxAge(*notifyMaxAge)
		}
	})
	if graceErr != nil {
		return nil, 0, false, fmt.Errorf("-shutdown-grace: %w", graceErr)
	}
	if spaceErr != nil {
		return nil, 0, false, fmt.Errorf("-min-free-space-mb: %w", spaceErr)
	}
	if ageErr != nil {
		return nil, 0, false, fmt.Errorf("-notification-max-age: %w", ageErr)
	}
	if err := cfg.Validate(); err != nil {
		return nil, 0, false, err
	}
	return cfg, level, false, nil
}
