// Package clientip resolves the network address of an HTTP client, optionally through
// trusted reverse proxies, and folds addresses into throttling keys. It is shared by
// the HTTP server and the auth service so both agree on what "one client" is.
package clientip

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ErrInvalidCIDR is returned by ParseTrusted for an entry that is neither a CIDR nor
// an IP address.
var ErrInvalidCIDR = errors.New("clientip: invalid CIDR or IP address")

// Trust says which peers may supply forwarding headers.
type Trust struct {
	// Prefixes are the trusted proxy networks. Forwarding headers are honoured only
	// when the connection's peer address is inside one of them.
	Prefixes []netip.Prefix
	// Legacy trusts forwarding headers from every peer. It is only meant for
	// installations that enabled security.trust_proxy_headers before
	// security.trusted_proxies existed and applies only while Prefixes is empty.
	Legacy bool
}

// ParseTrusted parses CIDRs and bare IP addresses (a /32 or /128). It returns
// canonical (masked, IPv4-mapped addresses unmapped) prefixes.
func ParseTrusted(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			bits := p.Bits()
			if p.Addr().Is4In6() && bits >= 96 {
				bits -= 96
			}
			out = append(out, netip.PrefixFrom(p.Addr().Unmap(), bits).Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidCIDR, truncate(e, 80))
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Enabled reports whether any peer can be trusted.
func (t Trust) Enabled() bool { return len(t.Prefixes) > 0 || t.Legacy }

// contains reports whether addr is a trusted proxy.
func (t Trust) contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range t.Prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// PeerTrusted reports whether the direct peer of r may supply forwarding headers.
func (t Trust) PeerTrusted(r *http.Request) bool {
	if len(t.Prefixes) == 0 {
		return t.Legacy
	}
	peer, ok := peerAddr(r.RemoteAddr)
	return ok && t.contains(peer)
}

// peerAddr parses the address of a RemoteAddr value (host:port or bare host).
func peerAddr(remote string) (netip.Addr, bool) {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if i := strings.IndexByte(host, '%'); i >= 0 { // IPv6 zone
		host = host[:i]
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// Resolve returns the client address of r. Forwarding headers are read only when
// the peer is trusted (PeerTrusted). Then X-Forwarded-For wins: every header line
// is joined and walked right to left, skipping trusted proxies, and the first
// untrusted address is the client (the leftmost one when all are trusted). Only
// without any X-Forwarded-For does X-Real-IP count. An unparsable value discards
// the headers and the peer address is returned, as it is for an untrusted peer.
func (t Trust) Resolve(r *http.Request) string {
	remote := r.RemoteAddr
	if h, _, err := net.SplitHostPort(remote); err == nil {
		remote = h
	}
	if !t.PeerTrusted(r) {
		return remote
	}
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		var entries []string
		for _, v := range values {
			entries = append(entries, strings.Split(v, ",")...)
		}
		for i := len(entries) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(entries[i]))
			if err != nil {
				return remote
			}
			a = a.Unmap()
			if i == 0 || !t.contains(a) {
				return a.String()
			}
		}
		return remote
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if a, err := netip.ParseAddr(v); err == nil {
			return a.Unmap().String()
		}
	}
	return remote
}

// Proto returns the scheme a trusted proxy reports in X-Forwarded-Proto (lower
// case, the rightmost value), or "" when the peer is untrusted or sent none.
func (t Trust) Proto(r *http.Request) string {
	if !t.PeerTrusted(r) {
		return ""
	}
	values := r.Header.Values("X-Forwarded-Proto")
	if len(values) == 0 {
		return ""
	}
	parts := strings.Split(values[len(values)-1], ",")
	return strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
}

// HasForwardingHeaders reports whether r carries X-Forwarded-For or X-Real-IP.
func HasForwardingHeaders(r *http.Request) bool {
	return len(r.Header.Values("X-Forwarded-For")) > 0 || r.Header.Get("X-Real-IP") != ""
}

// AddressKey returns the throttling key of a client address: an IPv6 address by its
// /64 (one host usually holds a whole /64), anything else as is.
func AddressKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}
