package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// tlsExpiryInterval is how often the expiry of the connections' TLS certificates
// is checked; a change of a connection checks it at once.
const tlsExpiryInterval = time.Hour

// tlsExpiryMonitor watches the x509 client certificates and CA certificates of
// the connections on the active instance: it keeps the expiry gauge current and
// announces each expiry threshold once per certificate.
type tlsExpiryMonitor struct {
	conns    *connections.Service
	ops      *operations.Service
	metrics  *metrics.Metrics
	logger   *slog.Logger
	interval time.Duration
	kick     chan struct{}
	tracker  *connections.TLSExpiryTracker
	now      func() time.Time
}

// newTLSExpiryMonitor returns a monitor that checks whenever kick receives (see
// kickFunc).
func newTLSExpiryMonitor(c *connections.Service, ops *operations.Service, m *metrics.Metrics, logger *slog.Logger, kick chan struct{}) *tlsExpiryMonitor {
	return &tlsExpiryMonitor{conns: c, ops: ops, metrics: m, logger: logger, interval: tlsExpiryInterval, kick: kick,
		tracker: connections.NewTLSExpiryTracker(), now: time.Now}
}

// kickFunc returns a function that asks for a check on kick without blocking.
func kickFunc(kick chan struct{}) func() {
	return func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

// Run checks at once, then every interval and whenever kicked, until ctx ends.
func (m *tlsExpiryMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.kick:
		}
	}
}

// check runs one check: it updates the gauge and announces new thresholds.
func (m *tlsExpiryMonitor) check(ctx context.Context) {
	now := m.now().UTC()
	statuses, err := m.conns.TLSCertStatuses(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("could not check the connection TLS certificate expiries", logsafe.Error(err))
		}
		return
	}
	if m.metrics != nil {
		samples := make([]metrics.TLSCertSample, 0, len(statuses))
		for _, st := range statuses {
			samples = append(samples, metrics.TLSCertSample{Connection: st.ConnectionID, Kind: st.Kind, NotAfter: st.NotAfter})
		}
		m.metrics.SetConnectionTLSExpiries(samples)
	}
	if announce := m.tracker.Announce(statuses); len(announce) > 0 && m.ops != nil {
		m.ops.TLSCertExpiryChecked(ctx, announce)
	}
}
