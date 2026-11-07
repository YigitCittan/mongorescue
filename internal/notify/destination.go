package notify

import (
	"syscall"

	"github.com/yigitcittan/mongorescue/internal/netguard"
)

// ErrBlockedDestination is returned when a notification would be sent to an address
// that is never a legitimate receiver: link-local addresses (including the cloud
// metadata services at 169.254.169.254 and fd00:ec2::254), unspecified, multicast
// and broadcast addresses, and, while security.outbound_deny_private is on, loopback
// and private networks too. Loopback and private networks stay allowed by default,
// since self-hosted receivers (chat servers, gateways, ntfy) commonly live there;
// only admins can configure channels. The check runs when a channel is saved (for
// IP literals) and again on every connection, after DNS resolution, so a hostname
// that resolves (or is rebound) to a blocked address is refused as well. The policy
// itself lives in internal/netguard, shared with the S3 driver.
var ErrBlockedDestination = netguard.ErrBlockedDestination

// lookupFunc resolves a host name to its addresses.
type lookupFunc = netguard.LookupFunc

// checkHost validates the host part of a destination against the process-wide
// policy (netguard.Default).
func checkHost(host string) error { return netguard.Default.CheckHost(host) }

// controlDestination refuses a socket whose remote address the process-wide policy
// refuses; it is the net.Dialer.Control of the HTTP and SMTP dialers.
func controlDestination(network, address string, c syscall.RawConn) error {
	return netguard.Default.Control(network, address, c)
}

// newGuardedDialer returns the dialer of the notifiers, resolving with lookup (the
// system resolver when nil).
func newGuardedDialer(lookup lookupFunc) *netguard.Dialer {
	return netguard.NewDialer(netguard.Default, lookup, SendTimeout, 0)
}
