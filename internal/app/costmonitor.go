package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/metrics"
)

// costSnapshotInterval is how often the storage usage snapshot is taken: the rows
// of today are rewritten each time, so a day keeps its last values.
const costSnapshotInterval = time.Hour

// costMonitor takes the daily storage usage snapshot on the active instance and
// keeps the mongorescue_storage_stored_bytes gauge current. A standby never runs
// it.
type costMonitor struct {
	svc      *cost.Service
	metrics  *metrics.Metrics
	logger   *slog.Logger
	interval time.Duration
}

// newCostMonitor returns the monitor of svc.
func newCostMonitor(svc *cost.Service, m *metrics.Metrics, logger *slog.Logger) *costMonitor {
	return &costMonitor{svc: svc, metrics: m, logger: logger, interval: costSnapshotInterval}
}

// Run snapshots at once, then every interval, until ctx ends.
func (m *costMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.snapshot(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// snapshot takes one snapshot and updates the gauge.
func (m *costMonitor) snapshot(ctx context.Context) {
	totals, err := m.svc.Snapshot(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("could not take the storage usage snapshot", logsafe.Error(err))
		}
		return
	}
	if m.metrics == nil {
		return
	}
	samples := make([]metrics.StoredBytesSample, 0, len(totals))
	for _, t := range totals {
		samples = append(samples, metrics.StoredBytesSample{Target: t.TargetID, Bytes: t.Bytes})
	}
	m.metrics.SetStoredBytes(samples)
}
