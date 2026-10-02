package metrics

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Outcomes of forwarding an audit log entry (auditlog.ForwardSent, ForwardFailed and
// ForwardDropped).
var auditForwardOutcomes = []string{"sent", "failed", "dropped"}

// auditSeries are the series of the audit log of every action.
type auditSeries struct {
	forwarded     *prometheus.CounterVec
	writeFailures prometheus.Counter
	syncWrites    prometheus.Counter
}

// newAuditSeries creates the audit log series and returns their collectors.
func (m *Metrics) newAuditSeries() []prometheus.Collector {
	m.audit = auditSeries{
		forwarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_forward_total",
			Help:      "Total number of audit log entries forwarded to the audit webhook by outcome (sent|failed|dropped).",
		}, []string{"outcome"}),
		writeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_write_failures_total",
			Help:      "Total number of audit log entries that could not be stored.",
		}),
		syncWrites: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_sync_writes_total",
			Help:      "Total number of audit log entries written on the request path because the write queue was full.",
		}),
	}
	for _, o := range auditForwardOutcomes {
		m.audit.forwarded.WithLabelValues(o)
	}
	queue := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "audit_queue_depth",
		Help:      "Number of audit log entries waiting to be written.",
	}, func() float64 {
		if fn := m.auditQueueSource.Load(); fn != nil {
			return float64((*fn)())
		}
		return 0
	})
	return []prometheus.Collector{m.audit.forwarded, m.audit.writeFailures, m.audit.syncWrites, queue}
}

// ObserveAuditForward counts one forwarded audit log entry by outcome; use it as
// auditlog.ForwarderConfig.Observe. Unknown outcomes are ignored.
func (m *Metrics) ObserveAuditForward(outcome string) {
	if slices.Contains(auditForwardOutcomes, outcome) {
		m.audit.forwarded.WithLabelValues(outcome).Inc()
	}
}

// IncAuditWriteFailures counts an audit log entry that could not be stored; use it
// as auditlog.Config.OnWriteFailure.
func (m *Metrics) IncAuditWriteFailures() { m.audit.writeFailures.Inc() }

// IncAuditSyncWrites counts an audit log entry written synchronously because the
// write queue was full; use it as auditlog.Config.OnSyncWrite.
func (m *Metrics) IncAuditSyncWrites() { m.audit.syncWrites.Inc() }

// SetAuditQueueSource sets the function reporting the audit log write queue depth.
func (m *Metrics) SetAuditQueueSource(fn func() int) { m.auditQueueSource.Store(&fn) }
