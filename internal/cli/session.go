package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
)

// maxKeyFileBytes bounds the API key file read.
const maxKeyFileBytes = 4096

// options are the shared flags of every command.
type options struct {
	url, apiKey, apiKeyFile string
	json, quiet, wait       bool
	timeout                 time.Duration
}

// session is the state of one command run.
type session struct {
	app            *App
	stdout, stderr io.Writer
	getenv         func(string) string
	opts           options
	// set holds the names of the flags given on the command line.
	set map[string]bool
	// baseURL is the validated URL once connect ran (for hints).
	baseURL string
}

// warnf writes a warning to stderr (also with --quiet: warnings are never noise).
func (s *session) warnf(format string, args ...any) {
	fmt.Fprintf(s.stderr, "mongorescue: warning: "+format+"\n", args...)
}

// infof writes progress or a note to stderr, unless --quiet.
func (s *session) infof(format string, args ...any) {
	if !s.opts.quiet {
		fmt.Fprintf(s.stderr, format+"\n", args...)
	}
}

// connect resolves the URL and the API key and returns a client.
func (s *session) connect() (*apiclient.Client, error) {
	raw := s.opts.url
	if !s.set["url"] {
		raw = s.getenv(EnvURL)
	}
	u, err := apiclient.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	s.baseURL = u.String()
	if u.Scheme == "http" && !apiclient.IsLoopback(u.Hostname()) {
		s.warnf("the API key is sent over plain HTTP to %s, which is not this machine; use https", u.Host)
	}
	key, err := s.resolveKey()
	if err != nil {
		return nil, err
	}
	version := s.app.Version
	if version == "" {
		version = "dev"
	}
	return apiclient.New(apiclient.Config{
		URL: u.String(), APIKey: key, UserAgent: "mongorescue-cli/" + version, Transport: "cli", HTTPClient: s.app.HTTPClient,
	})
}

// resolveKey returns the API key from, in this order: --api-key-file,
// MONGORESCUE_CLI_API_KEY_FILE, MONGORESCUE_CLI_API_KEY and --api-key (with a
// warning). It never reads MONGORESCUE_API_KEY or MONGORESCUE_API_KEY_FILE.
func (s *session) resolveKey() (string, error) {
	flagKey := strings.TrimSpace(s.opts.apiKey)
	var key, source string
	switch {
	case s.set["api-key-file"]:
		if strings.TrimSpace(s.opts.apiKeyFile) == "" {
			return "", usageErrorf("--api-key-file is empty")
		}
		k, err := readKeyFile(s.opts.apiKeyFile)
		if err != nil {
			return "", err
		}
		key, source = k, "--api-key-file"
	case strings.TrimSpace(s.getenv(EnvAPIKeyFile)) != "":
		k, err := readKeyFile(strings.TrimSpace(s.getenv(EnvAPIKeyFile)))
		if err != nil {
			return "", fmt.Errorf("%s: %w", EnvAPIKeyFile, err)
		}
		key, source = k, EnvAPIKeyFile
	case strings.TrimSpace(s.getenv(EnvAPIKey)) != "":
		key, source = strings.TrimSpace(s.getenv(EnvAPIKey)), EnvAPIKey
	case flagKey != "":
		s.warnf("--api-key is visible to other users in the process list; prefer --api-key-file or %s", EnvAPIKeyFile)
		return flagKey, nil
	default:
		return "", usageErrorf("no API key: set %s to a file holding a key (Settings → API keys), or set %s", EnvAPIKeyFile, EnvAPIKey)
	}
	if flagKey != "" {
		s.warnf("--api-key is ignored: %s takes precedence", source)
	}
	return key, nil
}

// readKeyFile reads an API key from path: the file's content without surrounding
// whitespace.
func readKeyFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the user names the key file.
	if err != nil {
		return "", fmt.Errorf("%w: API key file: %w", ErrUsage, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: API key file: %w", ErrUsage, err)
	}
	if len(b) > maxKeyFileBytes {
		return "", usageErrorf("API key file %s is larger than %d bytes", path, maxKeyFileBytes)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", usageErrorf("API key file %s is empty", path)
	}
	return key, nil
}

// parseInterspersed parses args with fs like fs.Parse, but also accepts flags after
// positional arguments ("restore ID --dry-run"). Everything after "--" is
// positional. It returns the positional arguments in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// Parse stops at the first positional argument or right after "--".
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// optBool is a boolean flag that remembers whether it was set.
type optBool struct {
	set, value bool
}

// String implements flag.Value.
func (b *optBool) String() string {
	if b == nil || !b.set {
		return ""
	}
	return strconv.FormatBool(b.value)
}

// Set implements flag.Value.
func (b *optBool) Set(v string) error {
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return errors.New("want true or false")
	}
	b.set, b.value = true, parsed
	return nil
}

// IsBoolFlag makes "--flag" mean "--flag=true".
func (b *optBool) IsBoolFlag() bool { return true }

// ptr returns a pointer to the value, or nil when the flag was not set.
func (b *optBool) ptr() *bool {
	if !b.set {
		return nil
	}
	v := b.value
	return &v
}

// csv splits a comma-separated flag value, dropping empty entries.
func csv(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// onlyOneID returns the single positional argument, the ID a command needs.
func onlyOneID(pos []string, what string) (string, error) {
	switch {
	case len(pos) == 0:
		return "", usageErrorf("missing the %s ID", what)
	case len(pos) > 1:
		return "", usageErrorf("unexpected arguments: %s", strings.Join(pos[1:], " "))
	}
	id := strings.TrimSpace(pos[0])
	if id == "" {
		return "", usageErrorf("empty %s ID", what)
	}
	return id, nil
}
