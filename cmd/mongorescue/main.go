// Package main is the entrypoint for MongoRescue CLI and embedded service.
//
// Only bootstrap options are read here (data directory, listen address, log level and
// the optional secret key); everything else is configured in the dashboard and stored
// in the database.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/config"
)

// Version metadata populated at build time via -ldflags.
var (
	Version = "1.0.0"
	Commit  = "dev"
	Date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
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
	logLevel := fs.String("log-level", "info", "Log level: debug, info, warn or error")
	showVersion := fs.Bool("version", false, "Print version information and exit")
	if err := fs.Parse(args); err != nil {
		return nil, 0, false, err
	}
	if *showVersion {
		return config.Default(), slog.LevelInfo, true, nil
	}
	if fs.NArg() > 0 {
		return nil, 0, false, fmt.Errorf("unexpected arguments: %v (everything except the bootstrap flags is configured in the dashboard)", fs.Args())
	}
	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		return nil, 0, false, err
	}
	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return nil, 0, false, err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "data-dir":
			cfg.DataDir = filepath.Clean(*dataDir)
		case "host":
			cfg.Host = *host
		case "port":
			cfg.Port = *port
		}
	})
	if err := cfg.Validate(); err != nil {
		return nil, 0, false, err
	}
	return cfg, level, false, nil
}
