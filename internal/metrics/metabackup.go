package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Results of a metadata backup.
const (
	// MetadataBackupOK is a snapshot that was stored.
	MetadataBackupOK = "ok"
	// MetadataBackupError is a snapshot that failed.
	MetadataBackupError = "error"
)

// metaBackupSeries are the series of the scheduled metadata self-backup. They carry
// no per-target labels: there is one metadata database.
type metaBackupSeries struct {
	total       *prometheus.CounterVec
	lastSuccess prometheus.Gauge
	lastSize    prometheus.Gauge
}

// newMetaBackupSeries creates the metadata backup series and returns their
// collectors.
func (m *Metrics) newMetaBackupSeries() []prometheus.Collector {
	m.metaBackup = metaBackupSeries{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "metadata_backups_total",
			Help:      "Total number of metadata database snapshots by result (ok|error).",
		}, []string{"result"}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_successful_metadata_backup_timestamp_seconds",
			Help:      "Unix timestamp of the last metadata database snapshot that was stored.",
		}),
		lastSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "metadata_backup_size_bytes",
			Help:      "Size in bytes of the last stored metadata database snapshot.",
		}),
	}
	for _, r := range []string{MetadataBackupOK, MetadataBackupError} {
		m.metaBackup.total.WithLabelValues(r)
	}
	return []prometheus.Collector{m.metaBackup.total, m.metaBackup.lastSuccess, m.metaBackup.lastSize}
}

// ObserveMetadataBackup records the outcome of a metadata snapshot: ok with the
// stored size and time, or a failure.
func (m *Metrics) ObserveMetadataBackup(ok bool, sizeBytes int64, at time.Time) {
	if !ok {
		m.metaBackup.total.WithLabelValues(MetadataBackupError).Inc()
		return
	}
	m.metaBackup.total.WithLabelValues(MetadataBackupOK).Inc()
	m.metaBackup.lastSuccess.Set(float64(at.UnixNano()) / 1e9)
	m.metaBackup.lastSize.Set(float64(sizeBytes))
}
