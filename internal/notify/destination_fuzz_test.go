package notify

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Reference ranges of the destination policy, written independently of blockedAddr.
var (
	refBlocked4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),          // "this network", unspecified
		netip.MustParsePrefix("169.254.0.0/16"),     // link-local, incl. 169.254.169.254
		netip.MustParsePrefix("224.0.0.0/4"),        // multicast
		netip.MustParsePrefix("255.255.255.255/32"), // broadcast
		netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	}
	refBlocked6 = []netip.Prefix{
		netip.MustParsePrefix("::/128"),            // unspecified
		netip.MustParsePrefix("fe80::/10"),         // link-local
		netip.MustParsePrefix("ff00::/8"),          // multicast
		netip.MustParsePrefix("fd00:ec2::254/128"), // AWS IMDS over IPv6
	}
	// nat64 is the well-known NAT64 prefix: 64:ff9b::a9fe:a9fe reaches 169.254.169.254
	// through a NAT64 gateway, so the embedded IPv4 address decides.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// refBlocked reports whether the policy must refuse ip: link-local (including the
// metadata services), unspecified, multicast and broadcast addresses, also inside
// IPv4-mapped and NAT64 IPv6 addresses. Loopback and private addresses are allowed.
func refBlocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	prefixes := refBlocked6
	if ip.Is4() {
		prefixes = refBlocked4
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// FuzzDestinationPolicy checks every enforcement point of the notification destination
// policy (channel validation, the dialer's resolution, the socket-level guard) against
// refBlocked for arbitrary IPv4 and IPv6 addresses, in plain and IPv4-mapped form.
func FuzzDestinationPolicy(f *testing.F) {
	for _, s := range []string{
		"169.254.169.254", "169.254.0.1", "100.100.100.200", "0.0.0.0", "0.1.2.3", "255.255.255.255", "224.0.0.1",
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "8.8.8.8",
		"::", "::1", "fe80::1", "ff02::1", "fd00:ec2::254", "fd00::1", "fc00::1", "2001:db8::1",
		"::ffff:169.254.169.254", "::ffff:127.0.0.1", "64:ff9b::a9fe:a9fe", "64:ff9b::808:808", "::a9fe:a9fe",
	} {
		f.Add(netip.MustParseAddr(s).AsSlice())
	}
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, raw []byte) {
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		want := refBlocked(ip)
		forms := []netip.Addr{ip}
		if ip.Is4() {
			forms = append(forms, netip.AddrFrom16(ip.As16())) // ::ffff:a.b.c.d
		}
		if want && !ip.Unmap().IsLoopback() && ip.Unmap().IsPrivate() && ip.Unmap() != netip.MustParseAddr("fd00:ec2::254") {
			t.Fatalf("reference blocks private address %s", ip)
		}
		for _, a := range forms {
			host := a.String()
			urlHost := host
			if a.Is6() {
				urlHost = "[" + host + "]"
			}
			if got := errors.Is(checkHost(urlHost), ErrBlockedDestination); got != want {
				t.Fatalf("checkHost(%s) blocked = %v; want %v", urlHost, got, want)
			}
			if got := validateWebhook(&WebhookConfig{URL: "https://" + urlHost + ":8443/hook"}) != nil; got != want {
				t.Fatalf("webhook to %s refused = %v; want %v", urlHost, got, want)
			}
			if got := validateEmail(&EmailConfig{Host: host, Port: 25, Security: SecurityNone, From: "a@example.com", To: []string{"b@example.com"}}) != nil; got != want {
				t.Fatalf("SMTP host %s refused = %v; want %v", host, got, want)
			}
			d := newGuardedDialer(func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{a}, nil })
			if _, err := d.resolve(ctx, "tcp", "receiver.example"); errors.Is(err, ErrBlockedDestination) != want || (err != nil && !want) {
				t.Fatalf("a name resolving to %s: err = %v; want blocked = %v", a, err, want)
			}
			if err := controlDestination("tcp", netip.AddrPortFrom(a, 443).String(), nil); errors.Is(err, ErrBlockedDestination) != want {
				t.Fatalf("socket to %s: err = %v; want blocked = %v", a, err, want)
			}
		}
	})
}

// inetAton parses h the way inet_aton does: one to four parts in decimal, octal
// (leading 0) or hex (0x), the last part filling the remaining bytes. A single
// trailing dot is ignored, as some resolvers do.
func inetAton(h string) (netip.Addr, bool) {
	parts := strings.Split(strings.TrimSuffix(h, "."), ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		base, digits := 10, p
		switch {
		case strings.HasPrefix(strings.ToLower(p), "0x"):
			base, digits = 16, p[2:]
			if digits == "" {
				digits = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		n, err := strconv.ParseUint(digits, base, 32)
		if err != nil || p == "" {
			return netip.Addr{}, false
		}
		nums[i] = n
	}
	var v uint64
	for i, n := range nums[:len(nums)-1] {
		if n > 0xff {
			return netip.Addr{}, false
		}
		v |= n << (8 * (3 - i))
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(4-len(nums)+1)) {
		return netip.Addr{}, false
	}
	v |= last
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// FuzzWebhookHostForms checks that an accepted webhook URL never names a blocked
// address in any spelling a resolver may read as an IP literal: IPv4 in any base or
// short form (inet_aton), IPv4-mapped or NAT64 IPv6, or an IPv6 literal with a zone.
func FuzzWebhookHostForms(f *testing.F) {
	for _, h := range []string{
		"169.254.169.254", "2852039166", "0xa9fea9fe", "0251.0376.0251.0376", "169.254.43518", "169.254.169.254.",
		"0x7f.1", "127.1", "127.0.0.1", "10.0.0.1", "[::ffff:a9fe:a9fe]", "[::ffff:169.254.169.254]",
		"[fe80::1%25eth0]", "[64:ff9b::a9fe:a9fe]", "metadata.google.internal", "hooks.example.com", "[::1]",
	} {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, host string) {
		raw := "https://" + host + "/hook"
		if validateWebhook(&WebhookConfig{URL: raw}) != nil {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted unparsable %q", raw)
		}
		h := u.Hostname()
		if strings.Contains(h, "%") {
			t.Fatalf("accepted a zoned address %q", h)
		}
		if ip, err := netip.ParseAddr(h); err == nil && refBlocked(ip) {
			t.Fatalf("accepted blocked address %q", h)
		}
		if ip, ok := inetAton(h); ok {
			if refBlocked(ip) {
				t.Fatalf("accepted %q, which inet_aton reads as blocked %s", h, ip)
			}
			if h != ip.String() {
				t.Fatalf("accepted %q, a non-canonical spelling of %s", h, ip)
			}
		}
		if ip := net.ParseIP(h); ip != nil {
			if a, ok := netip.AddrFromSlice(ip); ok && refBlocked(a) {
				t.Fatalf("accepted %q, which net.ParseIP reads as blocked %s", h, a)
			}
		}
	})
}
