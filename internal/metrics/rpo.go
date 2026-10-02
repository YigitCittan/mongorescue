package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RPOSample is the recovery point of one database of an enabled job, as the RPO
// checker last saw it.
type RPOSample struct {
	// JobID and Database identify the series.
	JobID    string
	Database string
	// Since is when the database's newest successful backup finished, or when the
	// job was created when it has none.
	Since time.Time
	// Target is the job's recovery point objective.
	Target time.Duration
}

// rpoCollector reports the job_rpo_* gauges from the checker's latest samples. Ages
// are computed at scrape time, so they grow between checks; the samples are
// replaced as a whole, so deleted jobs and databases leave no series behind.
type rpoCollector struct {
	mu      sync.RWMutex
	samples []RPOSample
	now     func() time.Time

	age, met, target *prometheus.Desc
}

// newRPOCollector returns the collector of the RPO gauges.
func newRPOCollector() *rpoCollector {
	labels := []string{"job", "database"}
	return &rpoCollector{
		now: time.Now,
		age: prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "job_rpo_seconds"),
			"Age in seconds of the newest successful backup of a database of an enabled job (since the job's creation when it has none).", labels, nil),
		met: prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "job_rpo_met"),
			"1 while that age is within the job's recovery point objective, 0 when the objective is missed.", labels, nil),
		target: prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "job_rpo_target_seconds"),
			"The job's recovery point objective in seconds (rpo_minutes, or the default from its schedule).", labels, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *rpoCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.age
	ch <- c.met
	ch <- c.target
}

// Collect implements prometheus.Collector.
func (c *rpoCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	for _, s := range c.samples {
		age := max(now.Sub(s.Since), 0)
		met := 0.0
		if age <= s.Target {
			met = 1
		}
		ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, age.Seconds(), s.JobID, s.Database)
		ch <- prometheus.MustNewConstMetric(c.met, prometheus.GaugeValue, met, s.JobID, s.Database)
		ch <- prometheus.MustNewConstMetric(c.target, prometheus.GaugeValue, s.Target.Seconds(), s.JobID, s.Database)
	}
}

// SetRPOSamples replaces the samples of the job_rpo_* gauges (one series per enabled
// job and database) with samples.
func (m *Metrics) SetRPOSamples(samples []RPOSample) {
	cp := make([]RPOSample, len(samples))
	copy(cp, samples)
	m.rpo.mu.Lock()
	m.rpo.samples = cp
	m.rpo.mu.Unlock()
}

// forgetRPOJob drops the RPO samples of jobID.
func (m *Metrics) forgetRPOJob(jobID string) {
	m.rpo.mu.Lock()
	defer m.rpo.mu.Unlock()
	kept := m.rpo.samples[:0:0]
	for _, s := range m.rpo.samples {
		if s.JobID != jobID {
			kept = append(kept, s)
		}
	}
	m.rpo.samples = kept
}
