package apiclient

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// TransportHeader names the client that sent a request (the MCP stdio bridge sends
// "stdio", the CLI "cli"), so that audit records can show it.
const TransportHeader = "X-MongoRescue-Transport"

// BearerTransport is an http.RoundTripper that adds an API key as a bearer token, plus
// the client's User-Agent and TransportHeader, to the requests it sends. The key is
// bound to one origin: requests to any other scheme or host never carry it, and an
// Authorization header set by the caller is always removed first. It remembers the
// status of the last response, so callers can explain refused connections.
//
// A BearerTransport must not be copied after first use.
type BearerTransport struct {
	// Base sends the requests; nil means http.DefaultTransport.
	Base http.RoundTripper
	// Key is the API key. It is never logged.
	Key string
	// Scheme and Host are the origin the key is sent to (compared case-insensitively).
	Scheme, Host string
	// UserAgent, when set, replaces the User-Agent header.
	UserAgent string
	// Transport, when set, is sent as TransportHeader.
	Transport string

	lastStatus atomic.Int32
}

// NewBearerTransport returns a BearerTransport over base that sends key to the origin
// of u only.
func NewBearerTransport(base http.RoundTripper, u *url.URL, key, userAgent, transport string) *BearerTransport {
	return &BearerTransport{Base: base, Key: key, Scheme: u.Scheme, Host: u.Host, UserAgent: userAgent, Transport: transport}
}

// RoundTrip implements http.RoundTripper.
func (t *BearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	if t.Key != "" && strings.EqualFold(r.URL.Scheme, t.Scheme) && strings.EqualFold(r.URL.Host, t.Host) {
		r.Header.Set("Authorization", "Bearer "+t.Key)
	}
	if t.Transport != "" {
		r.Header.Set(TransportHeader, t.Transport)
	}
	if t.UserAgent != "" {
		r.Header.Set("User-Agent", t.UserAgent)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(r)
	if resp != nil {
		t.lastStatus.Store(int32(resp.StatusCode)) //nolint:gosec // G115: HTTP status codes fit in int32.
	}
	return resp, err
}

// LastStatus returns the HTTP status of the last response, or 0 before the first.
func (t *BearerTransport) LastStatus() int {
	return int(t.lastStatus.Load())
}

// IsLoopback reports whether host (without a port) is localhost or a loopback
// address.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
