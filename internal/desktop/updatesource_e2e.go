//go:build desktop_e2e

package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// errE2EBaseURL is returned for an E2EUpdateBaseURLEnv value that is not an http
// URL on a loopback address without a path.
var errE2EBaseURL = errors.New("invalid " + E2EUpdateBaseURLEnv)

// defaultUpdateSource returns the release source of NewUpdater without
// UpdaterOptions.Source in a desktop_e2e build: GitHub, or, when
// E2EUpdateBaseURLEnv is set, the fake release server it names, from which release
// pages and assets are accepted too. An invalid value gives a source whose every
// call fails, so a misconfigured test never reaches GitHub.
func defaultUpdateSource(getenv func(string) string) UpdateSource {
	raw := strings.TrimSpace(getenv(E2EUpdateBaseURLEnv))
	if raw == "" {
		return &update.Checker{}
	}
	base, err := e2eBaseURL(raw)
	if err != nil {
		return failingSource{err: err}
	}
	return &update.Checker{BaseURL: base, DownloadPrefix: base + "/download/", ReleasesPrefix: base + "/releases/"}
}

// e2eBaseURL returns raw without a trailing slash when it is an http URL on a
// loopback address (127.0.0.0/8, ::1 or localhost) without credentials, path,
// query or fragment.
func e2eBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errE2EBaseURL, err)
	}
	if u.Scheme != "http" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q is not http://<loopback>:<port>", errE2EBaseURL, raw)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && !strings.EqualFold(host, "localhost") {
		return "", fmt.Errorf("%w: %q is not a loopback address", errE2EBaseURL, host)
	}
	return u.Scheme + "://" + u.Host, nil
}

// failingSource is an UpdateSource whose every call fails with err.
type failingSource struct{ err error }

// Check implements UpdateSource.
func (s failingSource) Check(context.Context, string) (update.Result, error) {
	return update.Result{}, s.err
}

// DownloadAsset implements UpdateSource.
func (s failingSource) DownloadAsset(context.Context, update.Result, update.Asset, string, update.Progress) (update.File, error) {
	return update.File{}, s.err
}
