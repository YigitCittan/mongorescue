package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// proxyServer returns a Server with the given trust settings.
func proxyServer(t *testing.T, legacy bool, trusted ...string) *Server {
	t.Helper()
	sec := settings.Defaults().Security
	sec.TrustProxyHeaders = legacy
	sec.TrustedProxies = trusted
	return &Server{cfg: bootConfig(), settings: newTestSettings(t, storetest.New(t), sec)}
}

func proxyRequest(remote string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header[http.CanonicalHeaderKey(k)] = v
	}
	return r
}

func TestClientIPTrustedProxies(t *testing.T) {
	s := proxyServer(t, false, "10.0.0.0/8")
	for _, tc := range []struct {
		name   string
		remote string
		header http.Header
		want   string
	}{
		{"spoofed XFF on a direct connection", "203.0.113.5:1", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "203.0.113.5"},
		{"multi-line XFF", "10.0.0.2:1", http.Header{"X-Forwarded-For": {"1.1.1.1", "198.51.100.1"}}, "198.51.100.1"},
		{"X-Real-IP-only proxy", "10.0.0.2:1", http.Header{"X-Real-IP": {"198.51.100.9"}}, "198.51.100.9"},
		{"invalid IP falls back to the peer", "10.0.0.2:1", http.Header{"X-Forwarded-For": {"bogus"}}, "10.0.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.clientIP(proxyRequest(tc.remote, tc.header)); got != tc.want {
				t.Errorf("clientIP = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestLegacyTrustProxyHeadersStillWorks pins the migration rule: the boolean alone
// trusts every peer, a non-empty trusted_proxies list narrows it.
func TestLegacyTrustProxyHeadersStillWorks(t *testing.T) {
	hdr := http.Header{"X-Forwarded-For": {"198.51.100.1"}}
	if got := proxyServer(t, true).clientIP(proxyRequest("203.0.113.5:1", hdr)); got != "198.51.100.1" {
		t.Errorf("legacy clientIP = %q", got)
	}
	if got := proxyServer(t, true, "10.0.0.0/8").clientIP(proxyRequest("203.0.113.5:1", hdr)); got != "203.0.113.5" {
		t.Errorf("legacy flag with a list must not trust other peers: %q", got)
	}
	if got := proxyServer(t, false, "10.0.0.0/8").clientIP(proxyRequest("10.0.0.2:1", hdr)); got != "198.51.100.1" {
		t.Errorf("a list alone trusts its peers: %q", got)
	}
}

func TestSecureRequestAndMCPFollowTrustedProxies(t *testing.T) {
	s := proxyServer(t, false, "10.0.0.0/8")
	https := http.Header{"X-Forwarded-Proto": {"https"}}
	if !s.secureRequest(proxyRequest("10.0.0.2:1", https)) {
		t.Error("https from a trusted proxy must be secure")
	}
	if s.secureRequest(proxyRequest("203.0.113.5:1", https)) {
		t.Error("X-Forwarded-Proto from an untrusted peer must be ignored")
	}
	if !s.trust().PeerTrusted(proxyRequest("10.0.0.2:1", nil)) || s.trust().PeerTrusted(proxyRequest("203.0.113.5:1", nil)) {
		t.Error("PeerTrusted must follow the list (used by the MCP loopback check)")
	}
}
