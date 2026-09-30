package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Defaults of a zero Checker.
const (
	// DefaultAPIBaseURL is the GitHub REST API.
	DefaultAPIBaseURL = "https://api.github.com"
	// Repository is the GitHub repository ("owner/name") releases are read from.
	Repository = "YigitCittan/mongorescue"
	// DefaultDownloadPrefix is the only URL prefix release assets are downloaded
	// from. GitHub redirects these URLs to its object storage; the client follows.
	DefaultDownloadPrefix = "https://github.com/" + Repository + "/releases/download/"
	// DefaultReleasesPrefix is the only URL prefix accepted for the release page.
	DefaultReleasesPrefix = "https://github.com/" + Repository + "/releases/"
	// DefaultCheckTimeout bounds a release check.
	DefaultCheckTimeout = 10 * time.Second
)

// Limits on what is read from the network.
const (
	maxReleaseJSON = 1 << 20 // the releases/latest response
	maxNotes       = 64 << 10
	maxChecksums   = 64 << 10
	// MaxAssetSize bounds a downloaded installer or archive.
	MaxAssetSize = 1 << 30
)

// assetPrefix starts the name of every desktop release asset.
const assetPrefix = "MongoRescue-desktop_"

// Sentinel errors.
var (
	// ErrNoRelease is returned when the repository has no published final release.
	ErrNoRelease = errors.New("no published release")
	// ErrUnexpectedStatus is returned for an HTTP status other than the expected one.
	ErrUnexpectedStatus = errors.New("unexpected HTTP status")
	// ErrUntrustedURL is returned for a download URL outside the repository's
	// release downloads.
	ErrUntrustedURL = errors.New("untrusted download URL")
	// ErrUnsupportedPlatform is returned when no desktop asset exists for the
	// operating system and architecture.
	ErrUnsupportedPlatform = errors.New("no desktop release for this platform")
	// ErrRateLimited is wrapped by the error of a check that GitHub refused because
	// its API rate limit is used up (see RateLimitError).
	ErrRateLimited = errors.New("GitHub API rate limit exceeded")
)

// RateLimitError reports a release lookup that GitHub refused for its rate limit
// (403 or 429 with X-RateLimit-Remaining: 0 or Retry-After). It wraps
// ErrRateLimited and ErrUnexpectedStatus.
type RateLimitError struct {
	// Status is the HTTP status line.
	Status string
	// Reset is when GitHub accepts requests again, from X-RateLimit-Reset or
	// Retry-After; zero when the response did not say.
	Reset time.Time
}

// Error implements error.
func (e *RateLimitError) Error() string {
	msg := "fetch latest release: " + ErrRateLimited.Error() + ": " + e.Status
	if !e.Reset.IsZero() {
		msg += " (until " + e.Reset.UTC().Format(time.RFC3339) + ")"
	}
	return msg
}

// Unwrap returns ErrRateLimited and ErrUnexpectedStatus.
func (e *RateLimitError) Unwrap() []error {
	return []error{ErrRateLimited, ErrUnexpectedStatus}
}

// rateLimit returns a *RateLimitError when resp is GitHub's refusal for its rate
// limit, or nil. now is the time the response was received.
func rateLimit(resp *http.Response, now time.Time) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if resp.Header.Get("X-RateLimit-Remaining") != "0" && retryAfter == "" {
		return nil // another 403, such as a blocked request
	}
	e := &RateLimitError{Status: resp.Status}
	if secs, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && secs >= 0 {
		e.Reset = now.Add(time.Duration(secs) * time.Second)
	} else if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
		e.Reset = time.Unix(reset, 0)
	}
	return e
}

// Asset is a downloadable release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Size is the size GitHub reports for the file, in bytes; 0 when unknown. It
	// only serves the download progress: the checksum decides what is accepted.
	Size int64 `json:"size"`
}

// Result is the outcome of a check.
type Result struct {
	// Current is the running version and Latest the newest final release.
	Current, Latest Version
	// Available reports whether Latest is higher than Current.
	Available bool
	// Mandatory reports whether Latest has a higher MAJOR version than Current and
	// is Installable: the running version must not be used any more. A major
	// release without installable files for this platform is only Available.
	Mandatory bool
	// Installable reports whether Available is set and the release has both the
	// desktop file for this platform and the checksums file, so Download can
	// fetch and verify it.
	Installable bool
	// Notes are the release notes (markdown), cut to a bounded length.
	Notes string
	// HTMLURL is the release page, always under DefaultReleasesPrefix (or the
	// Checker's ReleasesPrefix).
	HTMLURL string
	// Asset is the desktop file for this platform; zero when the release has none.
	Asset Asset
	// Portable is the Windows portable archive (MongoRescue.exe and tools/), which
	// the desktop app unpacks to update itself in place; zero on other platforms or
	// when the release has none.
	Portable Asset
	// ChecksumsURL is the release's checksums file; "" when it has none.
	ChecksumsURL string
}

// Checker looks up the latest release. A zero Checker uses the defaults; the fields
// exist for tests and must not be changed once it is in use. It is safe for
// concurrent use.
type Checker struct {
	// Client is used for the release lookup and the checksums file; nil means a
	// client with DefaultCheckTimeout.
	Client *http.Client
	// DownloadClient is used for the asset, which can take minutes; nil means a
	// client without an overall timeout (the context bounds it).
	DownloadClient *http.Client
	// BaseURL is the GitHub API base URL; "" means DefaultAPIBaseURL.
	BaseURL string
	// DownloadPrefix is the accepted asset URL prefix; "" means
	// DefaultDownloadPrefix.
	DownloadPrefix string
	// ReleasesPrefix is the accepted release page prefix; "" means
	// DefaultReleasesPrefix.
	ReleasesPrefix string
	// GOOS and GOARCH select the asset; "" means the running platform.
	GOOS, GOARCH string
}

// release is the part of a GitHub release read here.
type release struct {
	TagName    string `json:"tag_name"`
	Body       string `json:"body"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Check compares current with the latest final release. current must be a final
// version (see ParseVersion). It returns an error wrapping ErrNoRelease when there
// is no final release, including when the latest one is tagged as a pre-release.
func (c *Checker) Check(ctx context.Context, current string) (Result, error) {
	cur, err := ParseVersion(current)
	if err != nil {
		return Result{}, fmt.Errorf("current version: %w", err)
	}
	rel, err := c.fetchLatest(ctx, cur)
	if err != nil {
		return Result{}, err
	}
	if rel.Draft || rel.Prerelease {
		return Result{}, fmt.Errorf("%w: latest is %q, a draft or pre-release", ErrNoRelease, rel.TagName)
	}
	latest, err := ParseVersion(rel.TagName)
	if err != nil {
		return Result{}, fmt.Errorf("%w: latest tag %q: %w", ErrNoRelease, rel.TagName, err)
	}
	res := Result{
		Current:   cur,
		Latest:    latest,
		Available: latest.Compare(cur) > 0,
		Notes:     truncate(rel.Body, maxNotes),
		HTMLURL:   c.releasesPrefix() + "latest",
	}
	if c.trusted(rel.HTMLURL, c.releasesPrefix()) {
		res.HTMLURL = rel.HTMLURL
	}
	name, platErr := AssetName(c.goos(), c.goarch(), latest)
	portable, portableErr := PortableAssetName(c.goos(), c.goarch(), latest)
	checksums := ChecksumsName(latest)
	for _, a := range rel.Assets {
		if !c.trusted(a.URL, c.downloadPrefix()) {
			continue
		}
		switch {
		case a.Name == checksums:
			res.ChecksumsURL = a.URL
		case a.Name == name && platErr == nil:
			res.Asset = Asset{Name: a.Name, URL: a.URL, Size: a.Size}
		case a.Name == portable && portableErr == nil:
			res.Portable = Asset{Name: a.Name, URL: a.URL, Size: a.Size}
		}
	}
	res.Installable = res.Available && res.Asset.URL != "" && res.ChecksumsURL != ""
	res.Mandatory = res.Installable && latest.Major > cur.Major
	return res, nil
}

// fetchLatest reads GET /repos/<Repository>/releases/latest.
func (c *Checker) fetchLatest(ctx context.Context, cur Version) (release, error) {
	base := strings.TrimSuffix(c.BaseURL, "/")
	if base == "" {
		base = DefaultAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+Repository+"/releases/latest", nil)
	if err != nil {
		return release{}, fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", UserAgent(cur.String()))
	resp, err := c.client().Do(req)
	if err != nil {
		return release{}, fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return release{}, ErrNoRelease
	default:
		if err := rateLimit(resp, time.Now()); err != nil {
			return release{}, err
		}
		return release{}, fmt.Errorf("fetch latest release: %w: %s", ErrUnexpectedStatus, resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	return rel, nil
}

// AssetName returns the desktop asset for goos/goarch of version v: the installer on
// Windows, the universal app archive on macOS and the tarball on Linux. Other
// platforms return ErrUnsupportedPlatform.
func AssetName(goos, goarch string, v Version) (string, error) {
	var suffix string
	switch {
	case goos == "windows" && goarch == "amd64":
		suffix = "windows_amd64_installer.exe"
	case goos == "darwin":
		suffix = "macos_universal.zip"
	case goos == "linux" && goarch == "amd64":
		suffix = "linux_amd64.tar.gz"
	default:
		return "", fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, goos, goarch)
	}
	return assetPrefix + v.String() + "_" + suffix, nil
}

// PortableAssetName returns the Windows portable archive of version v, which holds
// MongoRescue.exe and tools/ at its root. Other platforms return
// ErrUnsupportedPlatform.
func PortableAssetName(goos, goarch string, v Version) (string, error) {
	if goos != "windows" || goarch != "amd64" {
		return "", fmt.Errorf("%w: no portable archive for %s/%s", ErrUnsupportedPlatform, goos, goarch)
	}
	return assetPrefix + v.String() + "_windows_amd64_portable.zip", nil
}

// ChecksumsName returns the name of the checksums file of version v.
func ChecksumsName(v Version) string {
	return assetPrefix + v.String() + "_checksums.txt"
}

// UserAgent returns the User-Agent sent by the checker.
func UserAgent(version string) string {
	return "MongoRescue/" + version
}

// trusted reports whether raw is an absolute URL whose scheme and host equal those of
// prefix and whose clean path starts with the path of prefix.
func (c *Checker) trusted(raw, prefix string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	p, err := url.Parse(prefix)
	if err != nil {
		return false
	}
	if u.Scheme != p.Scheme || !strings.EqualFold(u.Host, p.Host) {
		return false
	}
	if u.RawPath != "" || path.Clean(u.Path) != u.Path {
		return false // no encoded separators, "..", or duplicate slashes
	}
	return strings.HasPrefix(u.Path, p.Path) && len(u.Path) > len(p.Path)
}

// client returns the client for the release lookup.
func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return defaultClient
}

// downloadClient returns the client for the asset download.
func (c *Checker) downloadClient() *http.Client {
	if c.DownloadClient != nil {
		return c.DownloadClient
	}
	return defaultDownloadClient
}

// downloadPrefix returns the accepted asset URL prefix.
func (c *Checker) downloadPrefix() string {
	if c.DownloadPrefix != "" {
		return c.DownloadPrefix
	}
	return DefaultDownloadPrefix
}

// releasesPrefix returns the accepted release page prefix.
func (c *Checker) releasesPrefix() string {
	if c.ReleasesPrefix != "" {
		return c.ReleasesPrefix
	}
	return DefaultReleasesPrefix
}

// goos returns the operating system the asset is chosen for.
func (c *Checker) goos() string {
	if c.GOOS != "" {
		return c.GOOS
	}
	return runtime.GOOS
}

// goarch returns the architecture the asset is chosen for.
func (c *Checker) goarch() string {
	if c.GOARCH != "" {
		return c.GOARCH
	}
	return runtime.GOARCH
}

// Default clients. The download client has no overall timeout, only one for the
// response headers: the context bounds the transfer.
var (
	defaultClient         = &http.Client{Timeout: DefaultCheckTimeout, CheckRedirect: CheckRedirect}
	defaultDownloadClient = newDownloadClient()
)

// maxRedirects bounds the redirects followed for one request.
const maxRedirects = 5

// ErrRedirect is returned for a redirect CheckRedirect refuses.
var ErrRedirect = errors.New("refused redirect")

// CheckRedirect is the redirect policy of the default clients: at most
// maxRedirects hops, each to an https URL on github.com or a *.githubusercontent.com
// host (GitHub's release file storage).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: more than %d redirects", ErrRedirect, maxRedirects)
	}
	u := req.URL
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return fmt.Errorf("%w: to %s://%s", ErrRedirect, u.Scheme, u.Host)
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
		return fmt.Errorf("%w: to host %s", ErrRedirect, u.Host)
	}
	return nil
}

// newDownloadClient returns the default asset download client.
func newDownloadClient() *http.Client {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	t = t.Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t, CheckRedirect: CheckRedirect}
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	s = s[:n]
	return s + "…"
}
