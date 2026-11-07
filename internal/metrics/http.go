package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// UnmatchedRoute is the route label of a request that matches no registered route
// (unknown paths, wrong methods), so scanners cannot create series.
const UnmatchedRoute = "unmatched"

// otherMethod is the method label of a request method outside the standard set.
const otherMethod = "OTHER"

// httpBuckets cover API calls from a cache hit to an archive preview or a long
// download: 5 ms to 5 minutes.
var httpBuckets = []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 10, 30, 120, 300}

// newHTTPSeries creates the REST API request counter and duration histogram and
// returns their collectors. The labels are the registered route pattern (never the
// raw path), the method from a fixed set and the status code: all bounded.
func (m *Metrics) newHTTPSeries() []prometheus.Collector {
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "HTTP requests served by the API and dashboard, by route pattern (unmatched for unknown paths), method and status code. Scrapes of /metrics are not counted.",
	}, []string{"route", "method", "code"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "Duration of HTTP requests in seconds, by route pattern (unmatched for unknown paths) and method. Scrapes of /metrics are not counted.",
		Buckets:   httpBuckets,
	}, []string{"route", "method"})
	return []prometheus.Collector{m.httpRequests, m.httpDuration}
}

// ObserveHTTPRequest records one finished HTTP request (the server's request
// observer). route is the ServeMux pattern without its method ("/api/v1/jobs/{id}"),
// or "" for a request no route matches.
func (m *Metrics) ObserveHTTPRequest(route, method string, code int, d time.Duration) {
	if route == "" {
		route = UnmatchedRoute
	}
	method = boundedMethod(method)
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(d.Seconds())
}

// boundedMethod returns method if it is a standard HTTP method, else "OTHER".
func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	}
	return otherMethod
}
