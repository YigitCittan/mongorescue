package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// livenessSources feed the scheduler tick and settings warning gauges; dropped
// counts the heartbeat pings dropped because too many were pending.
type livenessSources struct {
	lastTick atomic.Pointer[func() time.Time]
	warnings atomic.Pointer[func() int]
	dropped  prometheus.Counter
}

// newLivenessSeries returns the collectors of the liveness series: the scheduler's
// last tick (mongorescue_scheduler_last_tick_timestamp_seconds), the number of
// active settings warnings (mongorescue_settings_warnings) and the dropped
// heartbeat pings (mongorescue_heartbeat_dropped_total).
func (m *Metrics) newLivenessSeries() []prometheus.Collector {
	lastTick := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "scheduler_last_tick_timestamp_seconds",
		Help:      "Unix timestamp of the scheduler's last liveness tick (every 30s while it runs; 0 before it starts).",
	}, func() float64 {
		if fn := m.liveness.lastTick.Load(); fn != nil {
			if t := (*fn)(); !t.IsZero() {
				return float64(t.UnixNano()) / 1e9
			}
		}
		return 0
	})
	warnings := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "settings_warnings",
		Help:      "Number of active settings warnings shown in the dashboard (encryption off, recovery kit missing or outdated, ...).",
	}, func() float64 {
		if fn := m.liveness.warnings.Load(); fn != nil {
			return float64((*fn)())
		}
		return 0
	})
	m.liveness.dropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "heartbeat_dropped_total",
		Help:      "Total number of job heartbeat pings dropped because too many pings were queued or in flight.",
	})
	return []prometheus.Collector{lastTick, warnings, m.liveness.dropped}
}

// IncHeartbeatDropped counts one dropped heartbeat ping; use it as
// heartbeat.Config.OnDrop.
func (m *Metrics) IncHeartbeatDropped() {
	m.liveness.dropped.Inc()
}

// SetSchedulerTickSource registers the function reporting the scheduler's last
// liveness tick (typically Scheduler.LastTick). It is safe to call at any time.
func (m *Metrics) SetSchedulerTickSource(fn func() time.Time) {
	if fn == nil {
		m.liveness.lastTick.Store(nil)
		return
	}
	m.liveness.lastTick.Store(&fn)
}

// SetSettingsWarningsSource registers the function reporting how many settings
// warnings are active (typically the length of settings.Service.Warnings). It is
// safe to call at any time.
func (m *Metrics) SetSettingsWarningsSource(fn func() int) {
	if fn == nil {
		m.liveness.warnings.Store(nil)
		return
	}
	m.liveness.warnings.Store(&fn)
}
