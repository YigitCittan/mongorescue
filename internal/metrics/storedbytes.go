package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// StoredBytesSample is the bytes MongoRescue stores on one storage target.
type StoredBytesSample struct {
	// Target is the storage target ID (names never become label values).
	Target string
	// Bytes is the total stored size of the archives, copies and PITR chunks.
	Bytes int64
}

// storedBytesCollector reports mongorescue_storage_stored_bytes from the samples
// of the latest usage snapshot. The samples are replaced as a whole, so a deleted
// target leaves no series behind, and the label is bounded by the number of
// targets: the per-job and per-database detail stays in the API. Only the active
// instance takes snapshots, and the family is not a standby family, so a standby
// exports nothing.
type storedBytesCollector struct {
	mu      sync.RWMutex
	samples []StoredBytesSample
	desc    *prometheus.Desc
}

// newStoredBytesCollector returns the collector of the stored bytes gauge.
func newStoredBytesCollector() *storedBytesCollector {
	return &storedBytesCollector{desc: prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "storage", "stored_bytes"),
		"Bytes MongoRescue stores on a storage target: backup archives, copies and PITR oplog chunks, as of the last daily usage snapshot (refreshed hourly). Not the provider's billed size.",
		[]string{"target"}, nil)}
}

// Describe implements prometheus.Collector.
func (c *storedBytesCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *storedBytesCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]bool, len(c.samples))
	for _, s := range c.samples {
		if seen[s.Target] {
			continue
		}
		seen[s.Target] = true
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.Bytes), s.Target)
	}
}

// SetStoredBytes replaces the samples of the mongorescue_storage_stored_bytes
// gauge; nil removes every series.
func (m *Metrics) SetStoredBytes(samples []StoredBytesSample) {
	m.storedBytes.mu.Lock()
	m.storedBytes.samples = append([]StoredBytesSample(nil), samples...)
	m.storedBytes.mu.Unlock()
}
