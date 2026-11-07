package clientip

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustTrust(t *testing.T, legacy bool, cidrs ...string) Trust {
	t.Helper()
	p, err := ParseTrusted(cidrs)
	if err != nil {
		t.Fatal(err)
	}
	return Trust{Prefixes: p, Legacy: legacy}
}

func request(remote string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header[http.CanonicalHeaderKey(k)] = v
	}
	return r
}

func TestResolve(t *testing.T) {
	proxy := mustTrust(t, false, "10.0.0.0/8", "fd00::/8")
	tests := []struct {
		name   string
		trust  Trust
		remote string
		header http.Header
		want   string
	}{
		{"direct connection ignores spoofed XFF", proxy, "203.0.113.5:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "203.0.113.5"},
		{"direct connection ignores spoofed X-Real-IP", proxy, "203.0.113.5:4000", http.Header{"X-Real-IP": {"198.51.100.1"}}, "203.0.113.5"},
		{"trusted proxy single XFF", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "198.51.100.1"},
		{"client-prepended entries are not trusted", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"1.1.1.1, 198.51.100.1"}}, "198.51.100.1"},
		{"multi-line XFF uses the last line", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"1.1.1.1", "198.51.100.1"}}, "198.51.100.1"},
		{"multi-line XFF skips trusted hops", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"1.1.1.1, 198.51.100.1", "10.0.0.7"}}, "198.51.100.1"},
		{"all trusted yields the leftmost", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"10.1.1.1, 10.0.0.7"}}, "10.1.1.1"},
		{"X-Real-IP without XFF", proxy, "10.0.0.2:4000", http.Header{"X-Real-IP": {"198.51.100.9"}}, "198.51.100.9"},
		{"XFF wins over X-Real-IP", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}, "X-Real-IP": {"198.51.100.9"}}, "198.51.100.1"},
		{"invalid XFF falls back to the peer", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"not-an-ip"}}, "10.0.0.2"},
		{"invalid entry mid-chain falls back to the peer", proxy, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"garbage, 10.0.0.7"}}, "10.0.0.2"},
		{"invalid X-Real-IP falls back to the peer", proxy, "10.0.0.2:4000", http.Header{"X-Real-IP": {"nope"}}, "10.0.0.2"},
		{"IPv4-mapped peer is trusted", proxy, "[::ffff:10.0.0.2]:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "198.51.100.1"},
		{"IPv6 proxy", proxy, "[fd00::1]:4000", http.Header{"X-Forwarded-For": {"2001:db8::1"}}, "2001:db8::1"},
		{"legacy trusts any peer, last entry", Trust{Legacy: true}, "203.0.113.5:4000", http.Header{"X-Forwarded-For": {"1.1.1.1", "198.51.100.1"}}, "198.51.100.1"},
		{"nothing trusted", Trust{}, "10.0.0.2:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "10.0.0.2"},
		{"list takes precedence over legacy", Trust{Prefixes: proxy.Prefixes, Legacy: true}, "203.0.113.5:4000", http.Header{"X-Forwarded-For": {"198.51.100.1"}}, "203.0.113.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trust.Resolve(request(tt.remote, tt.header)); got != tt.want {
				t.Errorf("Resolve = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestProto(t *testing.T) {
	proxy := mustTrust(t, false, "10.0.0.0/8")
	h := http.Header{"X-Forwarded-Proto": {"http", "HTTPS"}}
	if got := proxy.Proto(request("10.0.0.2:1", h)); got != "https" {
		t.Errorf("trusted proto = %q", got)
	}
	if got := proxy.Proto(request("203.0.113.5:1", h)); got != "" {
		t.Errorf("untrusted proto = %q; want empty", got)
	}
}

func TestParseTrusted(t *testing.T) {
	got, err := ParseTrusted([]string{" 10.1.2.3/8 ", "192.0.2.7", "2001:db8::1", "::ffff:10.0.0.0/104", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "2001:db8::1/128", "10.0.0.0/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("entry %d = %s; want %s", i, got[i], want[i])
		}
	}
	if _, err := ParseTrusted([]string{"10.0.0.0/33"}); !errors.Is(err, ErrInvalidCIDR) {
		t.Errorf("bad CIDR error = %v", err)
	}
	if _, err := ParseTrusted([]string{"proxy.example.com"}); !errors.Is(err, ErrInvalidCIDR) {
		t.Errorf("hostname error = %v", err)
	}
}

func TestAddressKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":            "192.0.2.1",
		"::ffff:192.0.2.1":     "192.0.2.1",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"not-an-ip":            "not-an-ip",
	} {
		if got := AddressKey(in); got != want {
			t.Errorf("AddressKey(%q) = %q; want %q", in, got, want)
		}
	}
}
