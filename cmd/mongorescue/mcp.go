package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/mcp"
)

// Environment variables of the mcp subcommand.
const (
	// EnvMCPAPIKey holds the API key the stdio bridge authenticates with.
	EnvMCPAPIKey = "MONGORESCUE_MCP_API_KEY" //nolint:gosec // G101: an environment variable name, not a credential.
	// EnvMCPURL overrides the default --url.
	EnvMCPURL = "MONGORESCUE_MCP_URL"
)

// mcpCommand is the name of the stdio bridge subcommand.
const mcpCommand = "mcp"

// runMCP runs "mongorescue mcp": an MCP server on stdin/stdout that forwards to the
// /mcp endpoint of a running MongoRescue instance. It never opens the data directory.
// stdout carries the protocol, so every log line goes to stderr. It returns the exit
// code.
func runMCP(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mongorescue mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: mongorescue mcp [--url URL]\n\n"+
			"Serves the Model Context Protocol on stdin/stdout for AI assistants and forwards it to a running\n"+
			"MongoRescue instance. Authenticate with an API key in %s.\n\n", EnvMCPAPIKey)
		fs.PrintDefaults()
	}
	defaultURL := getenv(EnvMCPURL)
	if defaultURL == "" {
		defaultURL = mcp.DefaultBridgeURL
	}
	url := fs.String("url", defaultURL, "Base URL of the MongoRescue instance (env "+EnvMCPURL+")")
	apiKey := fs.String("api-key", "", "API key (discouraged: visible in the process list; use "+EnvMCPAPIKey+")")
	logLevel := fs.String("log-level", "warn", "Log level on stderr: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}
	level, err := config.ParseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	key := getenv(EnvMCPAPIKey)
	if *apiKey != "" {
		key = *apiKey
		logger.Warn("--api-key is visible to other users in the process list; prefer the " + EnvMCPAPIKey + " environment variable")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcp.RunBridge(ctx, mcp.BridgeConfig{
		URL: *url, APIKey: key, Version: Version, Logger: logger, In: stdin, Out: stdout,
	}); err != nil {
		logger.Error("mcp bridge stopped", slog.Any("error", err))
		return 1
	}
	return 0
}
