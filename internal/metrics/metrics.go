// Package metrics exposes MongoRescue operational metrics in the Prometheus format.
//
// It uses a dedicated registry (never the global default) populated with Go runtime
// and process collectors plus MongoRescue-specific series fed from the events Bus,
// the notification Service and the scheduler. Label cardinality is bounded: the only
// variable label on backup series is the scheduled job ID, and manual (job-less)
// backups are reported as job="manual".
package metrics

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// namespace prefixes every MongoRescue metric.
const namespace = "mongorescue"

// ManualJobLabel is the job label value used for backups not tied to a scheduled job.
const ManualJobLabel = "manual"

// Status label values.
const (
	// StatusSucceeded labels successful backups and restores.
	StatusSucceeded = "succeeded"
	// StatusFailed labels failed backups and restores.
	StatusFailed = "failed"
)

// BuildInfo describes the running binary for the mongorescue_build_info series.
type BuildInfo struct {
	// Version is the release version.
	Version string
	// Commit is the VCS revision.
	Commit string
	// GoVersion is the Go toolchain version (runtime.Version()).
	GoVersion string
}

// Metrics owns the Prometheus registry and every MongoRescue collector.
type Metrics struct {
	registry *prometheus.Registry

	backupsTotal        *prometheus.CounterVec
	backupDuration      *prometheus.HistogramVec
	backupSize          *prometheus.GaugeVec
	lastSuccessBackup   *prometheus.GaugeVec
	restoresTotal       *prometheus.CounterVec
	notificationsTotal  *prometheus.CounterVec
	eventsDropped       prometheus.Counter
	scheduledJobsSource atomic.Pointer[func() int]
}

// New creates a Metrics instance with its own registry.
func New(info BuildInfo) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		backupsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "backups_total",
			Help:      "Total number of finished backups by job and status (succeeded|failed).",
		}, []string{"job", "status"}),
		backupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "backup_duration_seconds",
			Help:      "Duration of finished backups in seconds.",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
		}, []string{"job"}),
		backupSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "backup_size_bytes",
			Help:      "Size in bytes of the last successful backup archive per job.",
		}, []string{"job"}),
		lastSuccessBackup: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_successful_backup_timestamp_seconds",
			Help:      "Unix timestamp of the last successful backup per job.",
		}, []string{"job"}),
		restoresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "restores_total",
			Help:      "Total number of finished restores by status (succeeded|failed).",
		}, []string{"status"}),
		notificationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_total",
			Help:      "Total number of notification deliveries by channel type and outcome (success|failure|dropped).",
		}, []string{"channel_type", "status"}),
		eventsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "events_dropped_total",
			Help:      "Total number of events dropped because the event bus queue was full or stopped.",
		}),
	}

	scheduledJobs := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "scheduled_jobs",
		Help:      "Number of backup jobs currently registered with the scheduler.",
	}, func() float64 {
		if fn := m.scheduledJobsSource.Load(); fn != nil {
			return float64((*fn)())
		}
		return 0
	})

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information of the running MongoRescue binary (always 1).",
	}, []string{"version", "commit", "go_version"})
	buildInfo.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.backupsTotal,
		m.backupDuration,
		m.backupSize,
		m.lastSuccessBackup,
		m.restoresTotal,
		m.notificationsTotal,
		m.eventsDropped,
		scheduledJobs,
		buildInfo,
	)
	// Pre-create the fixed-cardinality series so dashboards see explicit zeros.
	for _, s := range []string{StatusSucceeded, StatusFailed} {
		m.restoresTotal.WithLabelValues(s)
	}
	return m
}

// Registry returns the dedicated registry (for tests and custom exporters).
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// Handler returns the HTTP handler serving the registry in the Prometheus exposition
// format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// SetScheduledJobsSource registers the function reporting the number of scheduled jobs
// (typically Scheduler.ActiveJobCount). It is safe to call at any time.
func (m *Metrics) SetScheduledJobsSource(fn func() int) {
	if fn == nil {
		m.scheduledJobsSource.Store(nil)
		return
	}
	m.scheduledJobsSource.Store(&fn)
}

// ObserveEvent is an events.Handler updating backup and restore series.
func (m *Metrics) ObserveEvent(_ context.Context, e events.Event) {
	switch e.Type {
	case events.BackupSucceeded, events.BackupFailed:
		job := e.JobID
		if job == "" {
			job = ManualJobLabel
		}
		status := StatusSucceeded
		if e.Type == events.BackupFailed {
			status = StatusFailed
		}
		m.backupsTotal.WithLabelValues(job, status).Inc()
		m.backupDuration.WithLabelValues(job).Observe(e.Duration.Seconds())
		if e.Type == events.BackupSucceeded {
			m.backupSize.WithLabelValues(job).Set(float64(e.SizeBytes))
			m.lastSuccessBackup.WithLabelValues(job).Set(float64(e.Time.UnixNano()) / 1e9)
		}
	case events.RestoreSucceeded:
		m.restoresTotal.WithLabelValues(StatusSucceeded).Inc()
	case events.RestoreFailed:
		m.restoresTotal.WithLabelValues(StatusFailed).Inc()
	}
}

// ForgetJob deletes every per-job series of jobID so that deleted jobs do not linger
// in scrapes (and label cardinality stays bounded by the live job set).
func (m *Metrics) ForgetJob(jobID string) {
	if jobID == "" {
		return
	}
	for _, s := range []string{StatusSucceeded, StatusFailed} {
		m.backupsTotal.DeleteLabelValues(jobID, s)
	}
	m.backupDuration.DeleteLabelValues(jobID)
	m.backupSize.DeleteLabelValues(jobID)
	m.lastSuccessBackup.DeleteLabelValues(jobID)
}

// ObserveNotification counts one notification outcome for a channel type.
func (m *Metrics) ObserveNotification(channelType, outcome string) {
	m.notificationsTotal.WithLabelValues(channelType, outcome).Inc()
}

// IncEventsDropped counts one dropped event; use it as the events.Bus drop hook.
func (m *Metrics) IncEventsDropped(events.Event) {
	m.eventsDropped.Inc()
}
