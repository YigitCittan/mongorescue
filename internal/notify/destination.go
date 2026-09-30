package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"syscall"
)

// ErrBlockedDestination is returned when a notification would be sent to an address
// that is never a legitimate receiver: link-local addresses (including the cloud
// metadata services at 169.254.169.254 and fd00:ec2::254), unspecified, multicast
// and broadcast addresses. Loopback and private networks stay allowed, since
// self-hosted receivers (chat servers, gateways, ntfy) commonly live there; only
// admins can configure channels. The check runs when a channel is saved (for IP
// literals) and again on every connection, after DNS resolution, so a hostname that
// resolves (or is rebound) to a blocked address is refused as well.
var ErrBlockedDestination = errors.New("notify: destination address is not allowed")

// metadataAddrs are cloud metadata endpoints outside the link-local ranges.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
}

// thisNetwork is 0.0.0.0/8, which some stacks route to the local host.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// blockedAddr reports whether a notification must never be sent to ip.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:169.254.169.254 is 169.254.169.254
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

// numericHost matches host names made only of numbers, in any base: "2130706433",
// "0x7f.1", "0177.0.0.1", "127.1". Resolvers disagree on what they mean (inet_aton
// reads them as IPv4 addresses), so only canonical dotted-decimal IPv4 is accepted.
var numericHost = regexp.MustCompile(`(?i)^(0x[0-9a-f]*|[0-9]+)(\.(0x[0-9a-f]*|[0-9]+)){0,3}\.?$`)

// checkHost validates the host part of a destination: an IP literal must not be
// blocked, and a numeric host must be canonical dotted-decimal IPv4.
func checkHost(host string) error {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.Contains(h, "%") {
		return fmt.Errorf("%w: IPv6 zones are not allowed", ErrBlockedDestination)
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if blockedAddr(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedDestination, ip)
		}
		return nil
	}
	if numericHost.MatchString(h) {
		return fmt.Errorf("%w: write IPv4 addresses in dotted decimal (a.b.c.d)", ErrBlockedDestination)
	}
	return nil
}

// lookupFunc resolves a host name to its addresses.
type lookupFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

// guardedDialer dials only destinations whose every resolved address is allowed
// (see blockedAddr). It resolves the name itself and dials the checked address, so a
// DNS answer cannot change between the check and the connection; the socket-level
// control check covers any path that bypasses the resolution.
type guardedDialer struct {
	lookup lookupFunc
	dialer *net.Dialer
}

// newGuardedDialer returns a guardedDialer resolving with lookup (the system
// resolver when nil).
func newGuardedDialer(lookup lookupFunc) *guardedDialer {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	return &guardedDialer{lookup: lookup, dialer: &net.Dialer{Timeout: SendTimeout, Control: controlDestination}}
}

// controlDestination refuses a socket whose remote address is blocked.
func controlDestination(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, address)
	}
	if blockedAddr(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, ap.Addr())
	}
	return nil
}

// DialContext resolves address, refuses it when any of its addresses is blocked and
// dials the allowed addresses in turn.
func (d *guardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if err = checkHost(host); err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else {
		ipNet := "ip"
		switch network {
		case "tcp4", "udp4":
			ipNet = "ip4"
		case "tcp6", "udp6":
			ipNet = "ip6"
		}
		if addrs, err = d.lookup(ctx, ipNet, host); err != nil {
			return nil, err
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if blockedAddr(a) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedDestination, host, a.Unmap())
		}
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
