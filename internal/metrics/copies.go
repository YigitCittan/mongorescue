package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Results of a backup copy attempt.
const (
	// CopyOK is a copy that was stored and matched the primary's checksum.
	CopyOK = "ok"
	// CopyMismatch is a copy whose bytes did not match the primary's checksum.
	CopyMismatch = "mismatch"
	// CopyError is a copy that failed for any other reason.
	CopyError = "error"
)

// copySeries are the series of backup copies (3-2-1). They carry no per-target
// labels, which keeps their cardinality fixed.
type copySeries struct {
	total       *prometheus.CounterVec
	queueSource atomic.Pointer[func() int]
}

// newCopySeries creates the copy series and returns their collectors.
func (m *Metrics) newCopySeries() []prometheus.Collector {
	m.copies.total = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "backup_copies_total",
		Help:      "Total number of backup copy attempts by result (ok|mismatch|error).",
	}, []string{"result"})
	for _, r := range []string{CopyOK, CopyMismatch, CopyError} {
		m.copies.total.WithLabelValues(r)
	}
	depth := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "backup_copy_queue_depth",
		Help:      "Number of backup copies waiting in the copy queue (pending or failed and retried).",
	}, func() float64 {
		if fn := m.copies.queueSource.Load(); fn != nil {
			return float64((*fn)())
		}
		return 0
	})
	return []prometheus.Collector{m.copies.total, depth}
}

// ObserveCopy records the result (CopyOK, CopyMismatch or CopyError) of one copy
// attempt.
func (m *Metrics) ObserveCopy(result string) {
	m.copies.total.WithLabelValues(result).Inc()
}

// SetCopyQueueSource registers the function reporting the depth of the copy queue.
func (m *Metrics) SetCopyQueueSource(fn func() int) {
	if fn == nil {
		m.copies.queueSource.Store(nil)
		return
	}
	m.copies.queueSource.Store(&fn)
}
