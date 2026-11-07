// Package netguard is the outbound destination policy shared by every client that
// connects to an address an administrator configured: notification channels, the
// heartbeat and audit forwarders, OIDC and S3 storage.
//
// Two rules apply. The first is always on: addresses that are never a legitimate
// destination (link-local, which includes the cloud metadata services, unspecified,
// multicast, broadcast and NAT64 forms of those) are refused. The second is a live
// option of the Policy: DenyPrivate also refuses loopback, private (RFC 1918),
// CGNAT (100.64.0.0/10) and unique local (fc00::/7) addresses, for deployments
// whose destinations are all public. Both are checked after DNS resolution, on the
// address that is dialled, and again on the socket, so a name that resolves (or is
// rebound) to a refused address is refused as well.
//
// The package depends on nothing else of MongoRescue.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrBlockedDestination is returned when a connection would go to an address the
// policy refuses: always link-local (including the cloud metadata services at
// 169.254.169.254 and fd00:ec2::254), unspecified, multicast and broadcast
// addresses, and with the Policy's DenyPrivate also loopback, private, CGNAT and
// unique local ones. Loopback and private networks are allowed by default, since
// self-hosted receivers and on-premises S3 commonly live there; only admins can
// configure destinations.
var ErrBlockedDestination = errors.New("netguard: destination address is not allowed")

// ErrInsecureEndpoint is returned when a request would use plain http while the
// Policy requires https.
var ErrInsecureEndpoint = errors.New("netguard: plain http is not allowed (security.require_https_endpoints is on)")

// Policy holds the live options of the destination policy. The zero value allows
// loopback and private networks and plain http; it is safe for concurrent use.
type Policy struct {
	denyPrivate  atomic.Bool
	requireHTTPS atomic.Bool
}

// Default is the process-wide policy: the application applies
// security.outbound_deny_private and security.require_https_endpoints to it.
var Default = &Policy{}

// SetDenyPrivate turns the refusal of loopback, private, CGNAT and unique local
// addresses on or off.
func (p *Policy) SetDenyPrivate(v bool) { p.denyPrivate.Store(v) }

// DenyPrivate reports whether private addresses are refused.
func (p *Policy) DenyPrivate() bool { return p.denyPrivate.Load() }

// SetRequireHTTPS turns the refusal of plain http on or off.
func (p *Policy) SetRequireHTTPS(v bool) { p.requireHTTPS.Store(v) }

// RequireHTTPS reports whether plain http is refused.
func (p *Policy) RequireHTTPS() bool { return p.requireHTTPS.Load() }

// metadataAddrs are cloud metadata endpoints outside the link-local ranges.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
}

var (
	// thisNetwork is 0.0.0.0/8, which some stacks route to the local host.
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")
	// nat64Prefix is the well-known NAT64 prefix (RFC 6052): on an IPv6-only network
	// with a NAT64 gateway, 64:ff9b::a9fe:a9fe reaches 169.254.169.254.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// cgnat is the shared address space of carrier-grade NAT (RFC 6598).
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	// siteLocal is the deprecated IPv6 site-local range.
	siteLocal = netip.MustParsePrefix("fec0::/10")
)

// normalize unmaps IPv4-mapped IPv6 addresses and translates NAT64 addresses to the
// IPv4 address they reach.
func normalize(ip netip.Addr) netip.Addr {
	ip = ip.Unmap() // ::ffff:169.254.169.254 is 169.254.169.254
	if ip.Is6() && nat64Prefix.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return ip
}

// Blocked reports whether ip is never a legitimate destination (the always-on rule).
func Blocked(ip netip.Addr) bool {
	ip = normalize(ip)
	switch {
	case !ip.IsValid(),
		ip.IsUnspecified(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}),
		ip.Is4() && thisNetwork.Contains(ip):
		return true
	}
	for _, m := range metadataAddrs {
		if ip == m {
			return true
		}
	}
	return false
}

// Private reports whether ip is a loopback, private (RFC 1918), CGNAT or unique
// local address (the rule of DenyPrivate).
func Private(ip netip.Addr) bool {
	ip = normalize(ip)
	return ip.IsLoopback() || ip.IsPrivate() || (ip.Is4() && cgnat.Contains(ip)) || (ip.Is6() && siteLocal.Contains(ip))
}

// refused reports whether the policy refuses ip, and a suffix naming the option
// when it is the reason.
func (p *Policy) refused(ip netip.Addr) (string, bool) {
	switch {
	case Blocked(ip):
		return "", true
	case p.DenyPrivate() && Private(ip):
		return " (security.outbound_deny_private is on)", true
	}
	return "", false
}

// numericHost matches host names made only of numbers, in any base: "2130706433",
// "0x7f.1", "0177.0.0.1", "127.1". Resolvers disagree on what they mean (inet_aton
// reads them as IPv4 addresses), so only canonical dotted-decimal IPv4 is accepted.
var numericHost = regexp.MustCompile(`(?i)^(0x[0-9a-f]*|[0-9]+)(\.(0x[0-9a-f]*|[0-9]+)){0,3}\.?$`)

// CheckHost validates the host part of a destination: an IP literal must not be
// refused, and a numeric host must be canonical dotted-decimal IPv4.
func (p *Policy) CheckHost(host string) error {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.Contains(h, "%") {
		return fmt.Errorf("%w: IPv6 zones are not allowed", ErrBlockedDestination)
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if why, bad := p.refused(ip); bad {
			return fmt.Errorf("%w: %s%s", ErrBlockedDestination, ip, why)
		}
		return nil
	}
	if numericHost.MatchString(h) {
		return fmt.Errorf("%w: write IPv4 addresses in dotted decimal (a.b.c.d)", ErrBlockedDestination)
	}
	return nil
}

// LookupFunc resolves a host name to its addresses.
type LookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// Dialer dials only destinations whose every resolved address the policy allows. It
// resolves the name itself and dials the checked address, so a DNS answer cannot
// change between the check and the connection; the socket-level control check
// covers any path that bypasses the resolution.
type Dialer struct {
	policy *Policy
	lookup LookupFunc
	dialer *net.Dialer
}

// NewDialer returns a Dialer enforcing policy (Default when nil), resolving with
// lookup (the system resolver when nil), with the given dial timeout and TCP
// keep-alive interval (zero keeps the net.Dialer default).
func NewDialer(policy *Policy, lookup LookupFunc, timeout, keepAlive time.Duration) *Dialer {
	if policy == nil {
		policy = Default
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	return &Dialer{policy: policy, lookup: lookup,
		dialer: &net.Dialer{Timeout: timeout, KeepAlive: keepAlive, Control: policy.Control}}
}

// Control refuses a socket whose remote address the policy refuses. It is the
// net.Dialer.Control of every dialer that carries the guard.
func (p *Policy) Control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, address)
	}
	if why, bad := p.refused(ap.Addr()); bad {
		return fmt.Errorf("%w: %s%s", ErrBlockedDestination, ap.Addr(), why)
	}
	return nil
}

// Resolve checks host (see CheckHost), resolves it on the IP network matching
// network and fails with ErrBlockedDestination when any address is refused.
func (d *Dialer) Resolve(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if err := d.policy.CheckHost(host); err != nil {
		return nil, err
	}
	if ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		return []netip.Addr{ip}, nil // checked by CheckHost
	}
	ipNet := "ip"
	switch network {
	case "tcp4", "udp4":
		ipNet = "ip4"
	case "tcp6", "udp6":
		ipNet = "ip6"
	}
	addrs, err := d.lookup(ctx, ipNet, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if why, bad := d.policy.refused(a); bad {
			return nil, fmt.Errorf("%w: %s resolves to %s%s", ErrBlockedDestination, host, a.Unmap(), why)
		}
	}
	return addrs, nil
}

// CheckProxiedTarget applies the policy to the target of a request sent through a
// proxy, which the dialer never sees. A name the local resolver cannot resolve is
// left to the proxy (common behind corporate proxies that resolve external names
// themselves); literals and resolvable names are checked.
func (d *Dialer) CheckProxiedTarget(ctx context.Context, host string) error {
	_, err := d.Resolve(ctx, "tcp", host)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return nil
	}
	return err
}

// DialContext resolves address, refuses it when any of its addresses is refused and
// dials the allowed addresses in turn.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := d.Resolve(ctx, network, host)
	if err != nil {
		return nil, err
	}
	var firstErr error
	for _, a := range addrs {
		conn, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// Guard installs the dialer on t: connections go through DialContext and, when a
// proxy is used (chosen by proxy, http.ProxyFromEnvironment when nil), the target
// host is resolved and checked before the request is handed to the proxy.
func (d *Dialer) Guard(t *http.Transport, proxy func(*http.Request) (*url.URL, error)) {
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	t.DialContext = d.DialContext
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		u, err := proxy(req)
		if err != nil || u == nil {
			return u, err
		}
		if err := d.CheckProxiedTarget(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
		return u, nil
	}
}

// httpsOnly wraps a RoundTripper and refuses plain http while the policy requires
// https. It sees every request, redirects included.
type httpsOnly struct {
	policy *Policy
	next   http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.policy.RequireHTTPS() && !strings.EqualFold(req.URL.Scheme, "https") {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s://%s", ErrInsecureEndpoint, req.URL.Scheme, req.URL.Host)
	}
	return h.next.RoundTrip(req)
}

// RequireHTTPSTransport wraps next so that, while policy (Default when nil)
// requires https, a request over plain http fails with ErrInsecureEndpoint before
// anything is sent. The setting is read for every request, so it applies to
// drivers that were built before it was turned on.
func RequireHTTPSTransport(policy *Policy, next http.RoundTripper) http.RoundTripper {
	if policy == nil {
		policy = Default
	}
	return httpsOnly{policy: policy, next: next}
}
