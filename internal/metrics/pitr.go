package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Results of an oplog chunk (mongorescue_pitr_chunks_total).
const (
	// ChunkOK is a chunk that was stored and committed.
	ChunkOK = "ok"
	// ChunkError is a chunk that failed (nothing was kept).
	ChunkError = "error"
)

// pitrSeries are the series of the PITR oplog collector, labelled by stream.
type pitrSeries struct {
	up          *prometheus.GaugeVec
	lag         *prometheus.GaugeVec
	lastChunk   *prometheus.GaugeVec
	chunks      *prometheus.CounterVec
	chunkBytes  *prometheus.CounterVec
	headroom    *prometheus.GaugeVec
	windowStart *prometheus.GaugeVec
	windowEnd   *prometheus.GaugeVec
	breaks      *prometheus.CounterVec
}

// newPITRSeries creates the PITR series and returns their collectors.
func (m *Metrics) newPITRSeries() []prometheus.Collector {
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Subsystem: "pitr", Name: name, Help: help}, []string{"stream"})
	}
	m.pitr = pitrSeries{
		up:        gauge("collector_up", "1 while the oplog collector of the stream stores chunks, 0 while it is failing or stopped."),
		lag:       gauge("lag_seconds", "Seconds between the replica set's newest write and the end of the last stored oplog chunk."),
		lastChunk: gauge("last_chunk_timestamp_seconds", "Oplog timestamp (seconds) of the end of the last stored chunk."),
		chunks: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Subsystem: "pitr", Name: "chunks_total",
			Help: "Oplog chunks by result (ok|error)."}, []string{"stream", "result"}),
		chunkBytes: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Subsystem: "pitr", Name: "chunk_bytes_total",
			Help: "Bytes of stored oplog chunks (after compression and encryption)."}, []string{"stream"}),
		headroom:    gauge("oplog_headroom_seconds", "Seconds between the oldest entry of the oplog and the collector's position: how long the collector may stop before entries are lost."),
		windowStart: gauge("window_start_timestamp_seconds", "Start of the newest point-in-time window (the consistent point of its oldest eligible base)."),
		windowEnd:   gauge("window_end_timestamp_seconds", "End of the newest point-in-time window (the end of its newest chunk)."),
		breaks: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Subsystem: "pitr", Name: "chain_breaks_total",
			Help: "Oplog chains ended by a gap, a replica set change or a divergence, by reason."}, []string{"stream", "reason"}),
	}
	p := m.pitr
	return []prometheus.Collector{p.up, p.lag, p.lastChunk, p.chunks, p.chunkBytes, p.headroom, p.windowStart, p.windowEnd, p.breaks}
}

// unix returns t as fractional Unix seconds.
func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// SetPITRCollectorUp records whether the collector of stream stores chunks.
func (m *Metrics) SetPITRCollectorUp(stream string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.pitr.up.WithLabelValues(stream).Set(v)
}

// ObservePITRChunk records a chunk of stream: ok with its stored size and the
// timestamp of its end, or a failure.
func (m *Metrics) ObservePITRChunk(stream string, ok bool, sizeBytes int64, end time.Time) {
	if !ok {
		m.pitr.chunks.WithLabelValues(stream, ChunkError).Inc()
		return
	}
	m.pitr.chunks.WithLabelValues(stream, ChunkOK).Inc()
	m.pitr.chunkBytes.WithLabelValues(stream).Add(float64(sizeBytes))
	m.pitr.lastChunk.WithLabelValues(stream).Set(unix(end))
}

// SetPITRLag records the lag and the oplog headroom of stream.
func (m *Metrics) SetPITRLag(stream string, lag, headroom time.Duration) {
	m.pitr.lag.WithLabelValues(stream).Set(lag.Seconds())
	m.pitr.headroom.WithLabelValues(stream).Set(headroom.Seconds())
}

// SetPITRWindow records the newest point-in-time window of stream; zero times
// remove the series (no window).
func (m *Metrics) SetPITRWindow(stream string, start, end time.Time) {
	if start.IsZero() || end.IsZero() {
		m.pitr.windowStart.DeleteLabelValues(stream)
		m.pitr.windowEnd.DeleteLabelValues(stream)
		return
	}
	m.pitr.windowStart.WithLabelValues(stream).Set(unix(start))
	m.pitr.windowEnd.WithLabelValues(stream).Set(unix(end))
}

// IncPITRChainBreak counts a chain of stream ended for reason (gap,
// replica_set_changed, diverged).
func (m *Metrics) IncPITRChainBreak(stream, reason string) {
	m.pitr.breaks.WithLabelValues(stream, reason).Inc()
}

// ForgetPITRStream drops every series of a deleted stream.
func (m *Metrics) ForgetPITRStream(stream string) {
	by := prometheus.Labels{"stream": stream}
	p := m.pitr
	for _, v := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		p.up, p.lag, p.lastChunk, p.chunks, p.chunkBytes, p.headroom, p.windowStart, p.windowEnd, p.breaks,
	} {
		v.DeletePartialMatch(by)
	}
}
