package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// TestWebhookDestinationPolicy pins which webhook URLs a channel may be saved with.
// Link-local and metadata addresses, unspecified, multicast and broadcast addresses
// and ambiguous numeric host encodings are refused; loopback and private networks are
// allowed by policy, because self-hosted receivers live there (see
// ErrBlockedDestination).
func TestWebhookDestinationPolicy(t *testing.T) {
	blocked := []string{
		// Cloud metadata and link-local.
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/",
		"http://[fe80::1]/",
		"http://[fe80::1%25eth0]/",
		"http://[fd00:ec2::254]/latest/meta-data/",
		"http://100.100.100.200/latest/meta-data/",
		// IPv4-mapped IPv6 forms of blocked addresses.
		"http://[::ffff:169.254.169.254]/",
		"http://[::ffff:a9fe:a9fe]/",
		"http://[0:0:0:0:0:ffff:169.254.169.254]/",
		// Unspecified, "this network", multicast, broadcast.
		"http://0.0.0.0/",
		"http://0.1.2.3/",
		"http://[::]/",
		"http://224.0.0.1/",
		"http://[ff02::1]/",
		"http://255.255.255.255/",
		// Decimal, octal, hex and short encodings (inet_aton forms).
		"http://2130706433/",
		"http://2852039166/",
		"http://0177.0.0.1/",
		"http://0251.0376.0251.0376/",
		"http://0x7f000001/",
		"http://0x7f.0x0.0x0.0x1/",
		"http://0xa9.0xfe.0xa9.0xfe/",
		"http://127.1/",
		"http://10.1/",
		"http://0/",
		"http://169.254.169.254./",
	}
	allowed := []string{
		"https://hooks.slack.com/services/T/B/x",
		"https://example.com:8443/hook",
		"http://8.8.8.8/",
		"http://localhost:8080/hook",
		"http://127.0.0.1/",
		"http://127.255.255.254/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://10.0.0.5/",
		"http://172.16.0.1/",
		"http://172.31.255.255/",
		"http://192.168.1.10/",
		"http://[fc00::1]/",
		"http://[fd12:3456::1]/",
		"http://100.64.0.1/",
		"http://1e100.net/",
		"http://123.example.com/",
	}
	for _, u := range blocked {
		err := validateWebhook(&WebhookConfig{URL: u})
		if !errors.Is(err, ErrInvalidChannelConfig) || !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("webhook %s: %v; want ErrBlockedDestination", u, err)
		}
	}
	for _, u := range allowed {
		if err := validateWebhook(&WebhookConfig{URL: u}); err != nil {
			t.Errorf("webhook %s: %v; want allowed", u, err)
		}
	}
	for _, host := range []string{"169.254.169.254", "0x7f000001", "2130706433", "::ffff:169.254.169.254"} {
		cfg := EmailConfig{Host: host, Port: 25, Security: SecurityNone, From: "a@example.com", To: []string{"b@example.com"}}
		if err := validateEmail(&cfg); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("smtp host %s: %v; want ErrBlockedDestination", host, err)
		}
	}
}

// fakeResolver answers lookups from a table and counts them; it never touches DNS.
type fakeResolver struct {
	answers map[string][]string
	calls   atomic.Int32
}

func (r *fakeResolver) lookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	var out []netip.Addr
	for _, a := range r.answers[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	if out == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

// TestNamesResolvingToBlockedAddressesAreRefused checks the connection-time guard: a
// harmless-looking host name that resolves to a metadata or link-local address (or to
// a mix of allowed and blocked ones, as in DNS rebinding) is refused before any
// connection is made, while a name resolving to an allowed private address connects
// to exactly the address that was checked.
func TestNamesResolvingToBlockedAddressesAreRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	res := &fakeResolver{answers: map[string][]string{
		"metadata.attacker.test": {"169.254.169.254"},
		"rebind.attacker.test":   {"127.0.0.1", "169.254.169.254"},
		"mapped.attacker.test":   {"::ffff:169.254.169.254"},
		"v6.attacker.test":       {"fd00:ec2::254"},
		"zero.attacker.test":     {"0.0.0.0"},
		"internal.corp.test":     {"127.0.0.1"},
	}}
	client := newGuardedHTTPClient(res.lookup)
	for _, host := range []string{"metadata.attacker.test", "rebind.attacker.test", "mapped.attacker.test", "v6.attacker.test", "zero.attacker.test"} {
		n := NewWebhookNotifier(WebhookConfig{URL: "http://" + net.JoinHostPort(host, port) + "/hook"}, client)
		err := n.Send(context.Background(), Message{Subject: "s", Body: "b"})
		if !errors.Is(err, ErrBlockedDestination) || !isPermanent(err) {
			t.Errorf("%s: %v; want a permanent ErrBlockedDestination", host, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a blocked destination was reached %d times", hits.Load())
	}
	n := NewWebhookNotifier(WebhookConfig{URL: "http://" + net.JoinHostPort("internal.corp.test", port) + "/hook"}, client)
	if err := n.Send(context.Background(), Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("private destination: %v; private networks are allowed", err)
	}
	if hits.Load() != 1 || res.calls.Load() < 6 {
		t.Fatalf("hits = %d, lookups = %d", hits.Load(), res.calls.Load())
	}
}

// TestSocketLevelGuard checks the last line of defence, run on every socket before
// it connects (HTTP and SMTP): it refuses blocked remote addresses whatever resolved
// them.
func TestSocketLevelGuard(t *testing.T) {
	for addr, blocked := range map[string]bool{
		"169.254.169.254:80":          true,
		"[fe80::1]:80":                true,
		"[::ffff:169.254.169.254]:80": true,
		"[fd00:ec2::254]:80":          true,
		"0.0.0.0:25":                  true,
		"not-an-address":              true,
		"127.0.0.1:25":                false,
		"10.1.2.3:587":                false,
		"[::1]:465":                   false,
		"93.184.216.34:443":           false,
	} {
		err := controlDestination("tcp", addr, nil)
		if blocked != errors.Is(err, ErrBlockedDestination) {
			t.Errorf("controlDestination(%s) = %v; blocked=%v", addr, err, blocked)
		}
	}
	// The SMTP dialer carries the guard: a blocked literal fails before connecting.
	d := &net.Dialer{Control: controlDestination}
	if _, err := d.DialContext(context.Background(), "tcp", "169.254.169.254:25"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("dial metadata: %v; want ErrBlockedDestination", err)
	}
}

// TestRedirectsAreNeverFollowed checks that a receiver answering with a redirect (to
// another host, a private address or the metadata service) does not make the client
// follow it, so neither the signed payload nor its credentials move elsewhere.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	t.Cleanup(target.Close)
	for _, location := range []string{target.URL + "/stolen", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/"} {
		for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, location, code)
			}))
			n := NewWebhookNotifier(WebhookConfig{URL: redirect.URL + "/hook", Secret: "hmac-secret", Headers: map[string]string{"Authorization": "Bearer tok"}}, nil)
			err := n.Send(context.Background(), Message{Subject: "s", Body: "b"})
			redirect.Close()
			if err == nil || !isPermanent(err) {
				t.Errorf("redirect %d to %s: %v; want a permanent failure", code, location, err)
			}
			if err != nil && strings.Contains(err.Error(), "hmac-secret") {
				t.Errorf("error leaks the secret: %v", err)
			}
		}
	}
	if followed.Load() != 0 {
		t.Fatalf("a redirect was followed %d times", followed.Load())
	}
}

// TestBlockedChannelsCannotBeSaved checks the policy through the service API.
func TestBlockedChannelsCannotBeSaved(t *testing.T) {
	for _, u := range []string{"http://169.254.169.254/", "http://0x7f000001/", "http://[::ffff:169.254.169.254]/"} {
		ch := &Channel{ID: "ch_x", Name: "x", Type: ChannelWebhook, Webhook: &WebhookConfig{URL: u}}
		if err := ch.Validate(); !errors.Is(err, ErrInvalidChannelConfig) || !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("channel with %s: %v", u, err)
		}
	}
}
