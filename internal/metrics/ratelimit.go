package metrics

import "github.com/prometheus/client_golang/prometheus"

// newAPIRateLimitSeries creates the REST API rate limit counter and returns its
// collector. Its labels are the kind of caller and the budget, never a key, user
// or address.
func (m *Metrics) newAPIRateLimitSeries() prometheus.Collector {
	m.apiRateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "api_rate_limited_total",
		Help:      "REST API requests refused with 429 by the rate limit, by caller (api_key|session|anonymous) and budget (general|work).",
	}, []string{"caller", "budget"})
	return m.apiRateLimited
}

// InitAPIRateLimited starts the rate limit series of every caller and budget at
// zero, so alerts on increase() see the first refusal.
func (m *Metrics) InitAPIRateLimited(callers, budgets []string) {
	for _, c := range callers {
		for _, b := range budgets {
			m.apiRateLimited.WithLabelValues(c, b)
		}
	}
}

// ObserveAPIRateLimited counts one REST API request refused by the rate limit (the
// server's rate limit observer).
func (m *Metrics) ObserveAPIRateLimited(caller, budget string) {
	m.apiRateLimited.WithLabelValues(caller, budget).Inc()
}
