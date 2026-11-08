package metrics

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// diskProbeTimeout bounds one disk source call made at scrape time.
const diskProbeTimeout = 5 * time.Second

// DiskTarget is the free space of one local storage target.
type DiskTarget struct {
	// Target is the storage target ID (the label of mongorescue_storage_free_bytes).
	Target string
	// FreeBytes is the space available to unprivileged users on the target's filesystem.
	FreeBytes uint64
}

// DiskUsage is a snapshot of the free space MongoRescue depends on.
type DiskUsage struct {
	// DataDirFree is the free space of the data directory's filesystem; it is
	// exported only while DataDirKnown is true.
	DataDirFree uint64
	// DataDirKnown reports that DataDirFree was measured.
	DataDirKnown bool
	// Targets lists the local storage targets whose free space was measured.
	Targets []DiskTarget
}

// DiskSource reports the current DiskUsage. It is called at scrape time with a
// bounded context and must be cheap (or cache its answer).
type DiskSource func(ctx context.Context) DiskUsage

// diskCollector exports the free-space gauges from the registered DiskSource. It
// starts no goroutine: it measures while Prometheus scrapes.
type diskCollector struct {
	source     atomic.Pointer[DiskSource]
	dataDir    *prometheus.Desc
	storageFre *prometheus.Desc
}

// newDiskCollector returns the collector of the free-space gauges.
func newDiskCollector() *diskCollector {
	return &diskCollector{
		dataDir: prometheus.NewDesc(namespace+"_data_dir_free_bytes",
			"Bytes available on the filesystem of the data directory (metadata database, secret key, logs).", nil, nil),
		storageFre: prometheus.NewDesc(namespace+"_storage_free_bytes",
			"Bytes available on the filesystem of a local storage target.", []string{"target"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *diskCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.dataDir
	ch <- c.storageFre
}

// Collect implements prometheus.Collector.
func (c *diskCollector) Collect(ch chan<- prometheus.Metric) {
	fn := c.source.Load()
	if fn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), diskProbeTimeout)
	defer cancel()
	u := (*fn)(ctx)
	if u.DataDirKnown {
		ch <- prometheus.MustNewConstMetric(c.dataDir, prometheus.GaugeValue, float64(u.DataDirFree))
	}
	seen := make(map[string]bool, len(u.Targets))
	for _, t := range u.Targets {
		if seen[t.Target] {
			continue
		}
		seen[t.Target] = true
		ch <- prometheus.MustNewConstMetric(c.storageFre, prometheus.GaugeValue, float64(t.FreeBytes), t.Target)
	}
}

// SetDiskSource registers the function reporting the free disk space; the
// mongorescue_data_dir_free_bytes and mongorescue_storage_free_bytes gauges are
// exported only while one is set. Standby HA instances do not export them (see
// StandbyHandler). The bytes MongoRescue stores on a target come from the usage
// snapshot, see SetStoredBytes.
func (m *Metrics) SetDiskSource(fn DiskSource) {
	if fn == nil {
		m.disk.source.Store(nil)
		return
	}
	m.disk.source.Store(&fn)
}
