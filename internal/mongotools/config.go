// Package mongotools contains helpers shared by the mongodump and mongorestore adapters.
//
// Its main purpose is keeping credentials out of subprocess argument lists: a connection
// string passed as "--uri=..." is visible to every local user via ps or /proc, so the URI
// is instead written to a private, short-lived YAML file consumed through "--config".
package mongotools

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrInvalidURI is returned when a connection string contains control characters that
// cannot be represented safely in the tools configuration file.
var ErrInvalidURI = errors.New("mongotools: connection uri contains control characters")

// configFilePerm restricts the config file to the current user.
const configFilePerm = 0o600

// ConfigFilePattern is the os.CreateTemp pattern used for tools configuration files.
// CleanupStale removes leftovers matching it.
const ConfigFilePattern = "mongorescue-tools-*.yaml"

// WriteURIConfig writes a mongo-tools (100.x) YAML configuration file containing uri to
// dir (os.TempDir when empty) with 0600 permissions, and returns the "--config=<path>"
// argument together with a cleanup function that removes the file. Callers must invoke
// cleanup once the subprocess has exited; it is safe to call more than once.
func WriteURIConfig(dir, uri string) (arg string, cleanup func(), err error) {
	if strings.ContainsFunc(uri, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", nil, ErrInvalidURI
	}

	f, err := os.CreateTemp(dir, ConfigFilePattern)
	if err != nil {
		return "", nil, fmt.Errorf("create tools config file: %w", err)
	}
	path := f.Name()
	cleanup = func() { _ = os.Remove(path) }

	if err = f.Chmod(configFilePerm); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("restrict tools config file permissions: %w", err)
	}

	// YAML single-quoted scalar: the only escape is doubling the quote character.
	content := "uri: '" + strings.ReplaceAll(uri, "'", "''") + "'\n"
	if _, err = f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write tools config file: %w", err)
	}
	if err = f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close tools config file: %w", err)
	}

	return "--config=" + path, cleanup, nil
}

// Default connection timeouts injected by WithConnectionDefaults. They bound how long a
// tool waits for an unreachable or unresolvable server before failing.
const (
	// DefaultServerSelectionTimeoutMS bounds server selection (and DNS resolution).
	DefaultServerSelectionTimeoutMS = 30000

	// DefaultConnectTimeoutMS bounds establishing each TCP/TLS connection.
	DefaultConnectTimeoutMS = 10000
)

// WithConnectionDefaults returns uri with serverSelectionTimeoutMS and connectTimeoutMS
// appended when the caller has not set them (option names match case-insensitively).
//
// It also appends authSource to a "mongodb://" URI that has credentials, no authSource
// and no authMechanism other than SCRAM-SHA-1 or SCRAM-SHA-256. The value is the URI path
// database, or "admin" when the path is empty, which is the Go driver's default. Without
// it the Database Tools authenticate against the --db database, so a connection that
// passes the driver-based test could fail with AuthenticationFailed. "mongodb+srv://"
// URIs are left alone because their SRV TXT records may provide authSource, and so are
// mechanisms that authenticate against $external.
//
// The result carries the same credentials as uri: pass it only to WriteURIConfig and
// never log or persist it. uri is assumed to satisfy mongouri.Validate, which forbids a
// raw '/' or '?' before the host list ends.
func WithConnectionDefaults(uri string) string {
	schemeEnd := strings.Index(uri, "://")
	if schemeEnd == -1 {
		return uri
	}
	rest := uri[schemeEnd+3:]

	query := ""
	if i := strings.IndexByte(rest, '?'); i != -1 {
		query = rest[i+1:]
	}
	present := make(map[string]bool)
	mechanism := ""
	for _, opt := range strings.Split(query, "&") {
		if key, value, ok := strings.Cut(opt, "="); ok {
			key = strings.ToLower(key)
			present[key] = true
			if key == "authmechanism" {
				mechanism = value
			}
		}
	}

	var add []string
	if source, ok := defaultAuthSource(uri[:schemeEnd], rest, mechanism, present["authsource"]); ok {
		add = append(add, "authSource="+url.QueryEscape(source))
	}
	if !present["serverselectiontimeoutms"] {
		add = append(add, fmt.Sprintf("serverSelectionTimeoutMS=%d", DefaultServerSelectionTimeoutMS))
	}
	if !present["connecttimeoutms"] {
		add = append(add, fmt.Sprintf("connectTimeoutMS=%d", DefaultConnectTimeoutMS))
	}
	if len(add) == 0 {
		return uri
	}
	extra := strings.Join(add, "&")

	switch {
	case strings.Contains(rest, "?"):
		if strings.HasSuffix(uri, "?") || strings.HasSuffix(uri, "&") {
			return uri + extra
		}
		return uri + "&" + extra
	case strings.Contains(rest, "/"):
		return uri + "?" + extra
	default:
		return uri + "/?" + extra
	}
}

// defaultAuthSource returns the authSource the Go driver would use for a URI with the
// given scheme and remainder after "://", and whether the Database Tools need it spelled
// out: the URI has credentials, no authSource, a SCRAM (or unset) authMechanism and the
// "mongodb" scheme. The source is the unescaped path database, or "admin".
func defaultAuthSource(scheme, rest, mechanism string, hasAuthSource bool) (string, bool) {
	if scheme != "mongodb" || hasAuthSource {
		return "", false
	}
	if mechanism != "" {
		m, err := url.QueryUnescape(mechanism)
		if err != nil {
			return "", false
		}
		if !strings.EqualFold(m, "SCRAM-SHA-1") && !strings.EqualFold(m, "SCRAM-SHA-256") {
			return "", false
		}
	}

	authority, path := rest, ""
	if i := strings.IndexAny(rest, "/?"); i != -1 {
		authority = rest[:i]
		if rest[i] == '/' {
			path = rest[i+1:]
		}
	}
	if !strings.Contains(authority, "@") {
		return "", false
	}
	if i := strings.IndexByte(path, '?'); i != -1 {
		path = path[:i]
	}
	if path == "" {
		return "admin", true
	}
	db, err := url.PathUnescape(path)
	if err != nil || db == "" {
		return "", false
	}
	return db, true
}

// CleanupStale removes leftover tools configuration files (ConfigFilePattern) in dir
// (os.TempDir when empty) whose modification time is older than olderThan. Such files
// can survive a crash or SIGKILL between WriteURIConfig and its cleanup; they contain
// credentials, so they are purged at startup. The tools read the file only when the
// subprocess starts, so removing files older than about a minute cannot disturb a
// running dump or restore.
//
// Only regular files are removed; symlinks and directories are skipped. Files that
// vanish concurrently or belong to another user (permission denied) are ignored.
// It returns the number of files removed and any other errors joined together.
func CleanupStale(dir string, olderThan time.Duration) (int, error) {
	if dir == "" {
		dir = os.TempDir()
	}

	matches, err := filepath.Glob(filepath.Join(dir, ConfigFilePattern))
	if err != nil {
		return 0, fmt.Errorf("glob stale tools config files: %w", err)
	}

	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var errs []error
	for _, path := range matches {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if !errors.Is(statErr, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("stat %s: %w", path, statErr))
			}
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil {
			if !errors.Is(rmErr, fs.ErrNotExist) && !errors.Is(rmErr, fs.ErrPermission) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, rmErr))
			}
			continue
		}
		removed++
	}

	return removed, errors.Join(errs...)
}
