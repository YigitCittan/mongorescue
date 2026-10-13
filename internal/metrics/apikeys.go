package metrics

import "github.com/prometheus/client_golang/prometheus"

// apiKeySeries are the API key expiry gauges. They carry no labels: key names and
// IDs never become label values.
type apiKeySeries struct {
	expiring prometheus.Gauge
	expired  prometheus.Gauge
}

// newAPIKeySeries creates the API key series and returns their collectors.
func (m *Metrics) newAPIKeySeries() []prometheus.Collector {
	m.apiKeys = apiKeySeries{
		expiring: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "api_keys_expiring",
			Help:      "API keys that expire within security.api_key_expiry_warning_days and have not expired yet.",
		}),
		expired: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "api_keys_expired",
			Help:      "Stored API keys past their expiry (they are refused; revoke them).",
		}),
	}
	return []prometheus.Collector{m.apiKeys.expiring, m.apiKeys.expired}
}

// SetAPIKeyExpiry reports how many API keys expire soon and how many have expired.
func (m *Metrics) SetAPIKeyExpiry(expiring, expired int) {
	m.apiKeys.expiring.Set(float64(expiring))
	m.apiKeys.expired.Set(float64(expired))
}
