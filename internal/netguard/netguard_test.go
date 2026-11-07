package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlockedAndPrivate(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
		private bool
	}{
		{"169.254.169.254", true, false},
		{"::ffff:169.254.169.254", true, false},
		{"64:ff9b::a9fe:a9fe", true, false}, // NAT64 to the metadata service
		{"64:ff9b::7f00:1", false, true},    // NAT64 to loopback
		{"fd00:ec2::254", true, true},       // also ULA
		{"100.100.100.200", true, true},     // also CGNAT
		{"fe80::1", true, false},
		{"0.0.0.0", true, false},
		{"224.0.0.1", true, false},
		{"255.255.255.255", true, false},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.1.2.3", false, true},
		{"172.16.0.1", false, true},
		{"192.168.1.1", false, true},
		{"100.64.0.1", false, true},
		{"100.127.255.254", false, true},
		{"100.128.0.1", false, false},
		{"fd12:3456::1", false, true},
		{"fec0::1", false, true},
		{"93.184.216.34", false, false},
		{"2606:4700::1111", false, false},
	}
	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.ip)
		if got := Blocked(ip); got != tt.blocked {
			t.Errorf("Blocked(%s) = %v; want %v", tt.ip, got, tt.blocked)
		}
		if got := Private(ip); got != tt.private {
			t.Errorf("Private(%s) = %v; want %v", tt.ip, got, tt.private)
		}
	}
}

func TestCheckHostHonoursDenyPrivate(t *testing.T) {
	p := &Policy{}
	for _, h := range []string{"127.0.0.1", "[::1]", "10.0.0.5", "minio.internal"} {
		if err := p.CheckHost(h); err != nil {
			t.Errorf("CheckHost(%s) = %v while private addresses are allowed", h, err)
		}
	}
	p.SetDenyPrivate(true)
	for _, h := range []string{"127.0.0.1", "[::1]", "10.0.0.5", "100.64.1.1", "[fd00::1]"} {
		err := p.CheckHost(h)
		if !errors.Is(err, ErrBlockedDestination) || !strings.Contains(err.Error(), "security.outbound_deny_private") {
			t.Errorf("CheckHost(%s) = %v; want a deny-private refusal", h, err)
		}
	}
	if err := p.CheckHost("minio.internal"); err != nil {
		t.Errorf("a name is checked after resolution, not by CheckHost: %v", err)
	}
	if err := p.CheckHost("169.254.169.254"); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("metadata literal: %v", err)
	}
}

func lookupOf(answers map[string][]string, calls *atomic.Int32) LookupFunc {
	return func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if calls != nil {
			calls.Add(1)
		}
		var out []netip.Addr
		for _, a := range answers[host] {
			out = append(out, netip.MustParseAddr(a))
		}
		if out == nil {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return out, nil
	}
}

func TestDialerRefusesNamesResolvingToRefusedAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	lookup := lookupOf(map[string][]string{
		"meta.test":    {"169.254.169.254"},
		"rebind.test":  {"127.0.0.1", "169.254.169.254"},
		"private.test": {"127.0.0.1"},
	}, nil)
	policy := &Policy{}
	d := NewDialer(policy, lookup, time.Second, 0)
	ctx := context.Background()

	for _, host := range []string{"meta.test", "rebind.test"} {
		if _, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port)); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: %v; want ErrBlockedDestination", host, err)
		}
	}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("private.test", port))
	if err != nil {
		t.Fatalf("private destination with the default policy: %v", err)
	}
	_ = conn.Close()

	policy.SetDenyPrivate(true)
	_, err = d.DialContext(ctx, "tcp", net.JoinHostPort("private.test", port))
	if !errors.Is(err, ErrBlockedDestination) || !strings.Contains(err.Error(), "security.outbound_deny_private") {
		t.Fatalf("private destination with deny-private: %v", err)
	}
	// The socket-level control refuses literals that bypass resolution.
	raw := &net.Dialer{Control: policy.Control}
	if _, err = raw.DialContext(ctx, "tcp", srv.Listener.Addr().String()); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("raw dial to loopback with deny-private: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused destination was reached %d times", hits.Load())
	}
}

func TestGuardChecksTheTargetOfAProxiedRequest(t *testing.T) {
	d := NewDialer(&Policy{}, lookupOf(map[string][]string{"meta.test": {"169.254.169.254"}}, nil), time.Second, 0)
	tr := &http.Transport{}
	proxyURL, _ := url.Parse("http://proxy.internal:3128")
	d.Guard(tr, func(*http.Request) (*url.URL, error) { return proxyURL, nil })

	req := httptest.NewRequest(http.MethodGet, "http://meta.test/latest", nil)
	if _, err := tr.Proxy(req); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("proxied metadata target: %v; want ErrBlockedDestination", err)
	}
	// A name the local resolver does not know is left to the proxy.
	req = httptest.NewRequest(http.MethodGet, "http://unknown.test/", nil)
	if u, err := tr.Proxy(req); err != nil || u == nil {
		t.Fatalf("unresolvable target: %v, %v; want the proxy", u, err)
	}
}

func TestRequireHTTPSTransport(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	policy := &Policy{}
	client := &http.Client{Transport: RequireHTTPSTransport(policy, http.DefaultTransport)}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("plain http while allowed: %v", err)
	}
	_ = resp.Body.Close()

	policy.SetRequireHTTPS(true)
	_, err = client.Get(srv.URL)
	if !errors.Is(err, ErrInsecureEndpoint) {
		t.Fatalf("plain http while https is required: %v; want ErrInsecureEndpoint", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d; the refused request must not be sent", hits.Load())
	}
}
