package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TLSCertSample is the earliest expiry of one kind of certificate of a connection.
type TLSCertSample struct {
	// Connection is the connection's ID (names never become label values).
	Connection string
	// Kind is "client" or "ca".
	Kind string
	// NotAfter is when the certificate that expires first expires.
	NotAfter time.Time
}

// tlsCertCollector reports mongorescue_connection_tls_cert_expiry_timestamp_seconds
// from the samples of the latest check. The samples are replaced as a whole, so a
// deleted connection or a removed certificate leaves no series behind. A standby
// never runs the check and exports nothing (the family is not a standby family).
type tlsCertCollector struct {
	mu      sync.RWMutex
	samples []TLSCertSample
	expiry  *prometheus.Desc
}

// newTLSCertCollector returns the collector of the certificate expiry gauge.
func newTLSCertCollector() *tlsCertCollector {
	return &tlsCertCollector{expiry: prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "connection_tls_cert_expiry_timestamp_seconds"),
		"Unix time at which the earliest certificate of a connection's TLS material expires: kind client is the x509 client certificate chain, ca the CA bundle.",
		[]string{"connection", "kind"}, nil)}
}

// Describe implements prometheus.Collector.
func (c *tlsCertCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.expiry }

// Collect implements prometheus.Collector.
func (c *tlsCertCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.samples {
		ch <- prometheus.MustNewConstMetric(c.expiry, prometheus.GaugeValue, float64(s.NotAfter.Unix()), s.Connection, s.Kind)
	}
}

// SetConnectionTLSExpiries replaces the samples of the
// mongorescue_connection_tls_cert_expiry_timestamp_seconds gauge.
func (m *Metrics) SetConnectionTLSExpiries(samples []TLSCertSample) {
	m.tlsCerts.mu.Lock()
	m.tlsCerts.samples = append([]TLSCertSample(nil), samples...)
	m.tlsCerts.mu.Unlock()
}
