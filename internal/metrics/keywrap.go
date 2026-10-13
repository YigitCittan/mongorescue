package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// keyWrapSeries are the series of the KMS wrapping of secret.key
// (internal/keywrap). Their labels are the provider and the operation (wrap or
// unwrap), never a key or key URI.
type keyWrapSeries struct {
	duration *prometheus.HistogramVec
	errors   *prometheus.CounterVec
	active   *prometheus.GaugeVec
}

// newKeyWrapSeries creates the key wrapping series and returns their collectors.
func (m *Metrics) newKeyWrapSeries() []prometheus.Collector {
	m.keyWrap = keyWrapSeries{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "key_wrap_duration_seconds",
			Help:      "Duration of the KMS calls that wrap and unwrap secret.key, by provider and operation (wrap|unwrap).",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"provider", "operation"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "key_wrap_errors_total",
			Help:      "Failed KMS calls that wrap or unwrap secret.key (each retry counts), by provider and operation (wrap|unwrap).",
		}, []string{"provider", "operation"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "key_wrapping_active",
			Help:      "1 when secret.key is wrapped by an external KMS (label provider), absent otherwise.",
		}, []string{"provider"}),
	}
	return []prometheus.Collector{m.keyWrap.duration, m.keyWrap.errors, m.keyWrap.active}
}

// SetKeyWrapping reports that secret.key is wrapped by provider: the
// key_wrapping_active gauge and explicit zeros of the error counters.
func (m *Metrics) SetKeyWrapping(provider string) {
	m.keyWrap.active.Reset()
	m.keyWrap.active.WithLabelValues(provider).Set(1)
	for _, op := range []string{"wrap", "unwrap"} {
		m.keyWrap.errors.WithLabelValues(provider, op)
	}
}

// ObserveKeyWrap records one KMS call (a keywrap.Observer): its duration and,
// when err is set, an error.
func (m *Metrics) ObserveKeyWrap(op, provider string, d time.Duration, err error) {
	m.keyWrap.duration.WithLabelValues(provider, op).Observe(d.Seconds())
	if err != nil {
		m.keyWrap.errors.WithLabelValues(provider, op).Inc()
	}
}
