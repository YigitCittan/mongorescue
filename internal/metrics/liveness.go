package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// livenessSources feed the scheduler tick and settings warning gauges.
type livenessSources struct {
	lastTick atomic.Pointer[func() time.Time]
	warnings atomic.Pointer[func() int]
}

// newLivenessSeries returns the collectors of the liveness gauges: the scheduler's
// last tick (mongorescue_scheduler_last_tick_timestamp_seconds) and the number of
// active settings warnings (mongorescue_settings_warnings).
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
	return []prometheus.Collector{lastTick, warnings}
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
