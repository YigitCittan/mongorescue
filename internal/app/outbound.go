package app

import (
	"github.com/yigitcittan/mongorescue/internal/netguard"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// applyOutboundPolicy copies security.outbound_deny_private and
// security.require_https_endpoints of svc to the process-wide destination policy
// (netguard.Default) now and after every change of the settings, so the notifiers,
// the heartbeat and audit forwarders, OIDC and the S3 drivers follow the dashboard
// without a restart.
func applyOutboundPolicy(svc *settings.Service) {
	apply := func() {
		sec := svc.Current().Security
		netguard.Default.SetDenyPrivate(sec.OutboundDenyPrivate)
		netguard.Default.SetRequireHTTPS(sec.RequireHTTPSEndpoints)
	}
	apply()
	svc.OnChange(func([]string) { apply() })
}
