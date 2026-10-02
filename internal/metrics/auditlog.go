package metrics

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Outcomes of forwarding an audit log entry (auditlog.ForwardSent, ForwardFailed and
// ForwardDropped).
var auditForwardOutcomes = []string{"sent", "failed", "dropped"}

// newAuditForwardSeries creates the audit forwarding counter and returns it.
func (m *Metrics) newAuditForwardSeries() prometheus.Collector {
	m.auditForwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "audit_forward_total",
		Help:      "Total number of audit log entries forwarded to the audit webhook by outcome (sent|failed|dropped).",
	}, []string{"outcome"})
	for _, o := range auditForwardOutcomes {
		m.auditForwarded.WithLabelValues(o)
	}
	return m.auditForwarded
}

// ObserveAuditForward counts one forwarded audit log entry by outcome; use it as
// auditlog.ForwarderConfig.Observe. Unknown outcomes are ignored.
func (m *Metrics) ObserveAuditForward(outcome string) {
	if slices.Contains(auditForwardOutcomes, outcome) {
		m.auditForwarded.WithLabelValues(outcome).Inc()
	}
}
