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
	// BulkSkipped labels bulk items the action did not apply to.
	BulkSkipped = "skipped"
	// StatusCancelled labels cancelled backups and restores.
	StatusCancelled = "cancelled"
)

// Run kinds of the active_runs gauge.
const (
	// KindBackup labels backup runs.
	KindBackup = "backup"
	// KindRestore labels restore runs.
	KindRestore = "restore"
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
	databaseBackups     *prometheus.CounterVec
	databaseSize        *prometheus.GaugeVec
	databaseLastSuccess *prometheus.GaugeVec
	jobRuns             *prometheus.CounterVec
	jobRunDuration      *prometheus.HistogramVec
	restoresTotal       *prometheus.CounterVec
	restoreDuration     prometheus.Histogram
	notificationsTotal  *prometheus.CounterVec
	eventsDropped       prometheus.Counter
	mcpCalls            *prometheus.CounterVec
	bulkOperations      *prometheus.CounterVec
	bulkItems           *prometheus.CounterVec
	scheduledJobsSource atomic.Pointer[func() int]
	activeRunsSource    atomic.Pointer[func(kind string) int]

	// Integrity series (see integrity.go).
	integrity integritySeries
	// Metadata self-backup series (see metabackup.go).
	metaBackup metaBackupSeries
	// Audit log series (see auditlog.go) and the write queue depth source.
	audit            auditSeries
	auditQueueSource atomic.Pointer[func() int]
	// Recovery point objective gauges (see rpo.go).
	rpo *rpoCollector
	// Scheduler tick and settings warning sources (see liveness.go).
	liveness livenessSources
}

// New creates a Metrics instance with its own registry.
func New(info BuildInfo) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		backupsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "backups_total",
			Help:      "Total number of finished backups by job and status (succeeded|failed|cancelled).",
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
		databaseBackups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "database_backups_total",
			Help:      "Total number of finished backups by job, database and status (succeeded|failed|cancelled).",
		}, []string{"job", "database", "status"}),
		databaseSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "database_backup_size_bytes",
			Help:      "Size in bytes of the last successful backup archive per job and database.",
		}, []string{"job", "database"}),
		databaseLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "database_last_successful_backup_timestamp_seconds",
			Help:      "Unix timestamp of the last successful backup per job and database.",
		}, []string{"job", "database"}),
		jobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "job_runs_total",
			Help:      "Total number of finished job runs (all their databases) by job and status (ok|partial|failed|cancelled).",
		}, []string{"job", "status"}),
		jobRunDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "job_run_duration_seconds",
			Help:      "Duration of finished job runs (all their databases) in seconds.",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400, 28800},
		}, []string{"job"}),
		restoresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "restores_total",
			Help:      "Total number of finished restores by status (succeeded|failed|cancelled).",
		}, []string{"status"}),
		restoreDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "restore_duration_seconds",
			Help:      "Duration of finished restores in seconds.",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
		}),
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
		mcpCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mcp_calls_total",
			Help:      "Total number of MCP tool calls by tool and result (ok|error|denied|rate_limited).",
		}, []string{"tool", "result"}),
		bulkOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bulk_operations_total",
			Help:      "Total number of bulk operations run (not dry runs) by resource (backups|restores|jobs) and action.",
		}, []string{"resource", "action"}),
		bulkItems: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bulk_items_total",
			Help:      "Total number of items processed by bulk operations by resource, action and outcome (succeeded|skipped|failed).",
		}, []string{"resource", "action", "outcome"}),
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

	activeRuns := make([]prometheus.Collector, 0, 2)
	for _, kind := range []string{KindBackup, KindRestore} {
		activeRuns = append(activeRuns, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "active_runs",
			Help:        "Number of backups and restores running now, by kind (backup|restore).",
			ConstLabels: prometheus.Labels{"kind": kind},
		}, func() float64 {
			if fn := m.activeRunsSource.Load(); fn != nil {
				return float64((*fn)(kind))
			}
			return 0
		}))
	}

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
		m.databaseBackups,
		m.databaseSize,
		m.databaseLastSuccess,
		m.jobRuns,
		m.jobRunDuration,
		m.restoresTotal,
		m.restoreDuration,
		m.notificationsTotal,
		m.eventsDropped,
		m.mcpCalls,
		m.bulkOperations,
		m.bulkItems,
		scheduledJobs,
		m.newIntegritySeries(),
		buildInfo,
	)
	m.registry.MustRegister(activeRuns...)
	m.registry.MustRegister(m.newMetaBackupSeries()...)
	m.registry.MustRegister(m.newAuditSeries()...)
	m.registry.MustRegister(m.newLivenessSeries()...)
	m.rpo = newRPOCollector()
	m.registry.MustRegister(m.rpo)
	// Pre-create the fixed-cardinality series so dashboards see explicit zeros.
	for _, s := range []string{StatusSucceeded, StatusFailed, StatusCancelled} {
		m.restoresTotal.WithLabelValues(s)
		m.backupsTotal.WithLabelValues(ManualJobLabel, s)
	}
	return m
}

// SetActiveRunsSource registers the function reporting how many runs of a kind
// (KindBackup, KindRestore) are active. It is safe to call at any time.
func (m *Metrics) SetActiveRunsSource(fn func(kind string) int) {
	if fn == nil {
		m.activeRunsSource.Store(nil)
		return
	}
	m.activeRunsSource.Store(&fn)
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
	case events.BackupSkipped:
		// A skipped run is counted as a run, never as a backup.
		job := e.JobID
		if job == "" {
			job = ManualJobLabel
		}
		m.jobRuns.WithLabelValues(job, "skipped").Inc()
	case events.BackupSucceeded, events.BackupFailed, events.BackupCancelled:
		job := e.JobID
		if job == "" {
			job = ManualJobLabel
		}
		if e.Run != nil {
			m.jobRuns.WithLabelValues(job, e.Run.Status).Inc()
			m.jobRunDuration.WithLabelValues(job).Observe(e.Duration.Seconds())
			if e.Run.Multi {
				// The summary of a multi-database run: its databases were counted
				// by their own events.
				return
			}
		}
		status := StatusSucceeded
		switch e.Type {
		case events.BackupFailed:
			status = StatusFailed
		case events.BackupCancelled:
			status = StatusCancelled
		}
		m.backupsTotal.WithLabelValues(job, status).Inc()
		m.backupDuration.WithLabelValues(job).Observe(e.Duration.Seconds())
		if e.Database != "" {
			m.databaseBackups.WithLabelValues(job, e.Database, status).Inc()
		}
		if e.Type == events.BackupSucceeded {
			m.backupSize.WithLabelValues(job).Set(float64(e.SizeBytes))
			m.lastSuccessBackup.WithLabelValues(job).Set(float64(e.Time.UnixNano()) / 1e9)
			if e.Database != "" {
				m.databaseSize.WithLabelValues(job, e.Database).Set(float64(e.SizeBytes))
				m.databaseLastSuccess.WithLabelValues(job, e.Database).Set(float64(e.Time.UnixNano()) / 1e9)
			}
		}
	case events.RestoreSucceeded:
		m.restoresTotal.WithLabelValues(StatusSucceeded).Inc()
		m.restoreDuration.Observe(e.Duration.Seconds())
	case events.RestoreFailed:
		m.restoresTotal.WithLabelValues(StatusFailed).Inc()
		m.restoreDuration.Observe(e.Duration.Seconds())
	case events.RestoreCancelled:
		m.restoresTotal.WithLabelValues(StatusCancelled).Inc()
		m.restoreDuration.Observe(e.Duration.Seconds())
	case events.BulkCompleted:
		if b := e.Bulk; b != nil {
			m.bulkOperations.WithLabelValues(b.Resource, b.Action).Inc()
			m.bulkItems.WithLabelValues(b.Resource, b.Action, StatusSucceeded).Add(float64(b.Succeeded))
			m.bulkItems.WithLabelValues(b.Resource, b.Action, BulkSkipped).Add(float64(b.Skipped))
			m.bulkItems.WithLabelValues(b.Resource, b.Action, StatusFailed).Add(float64(b.Failed))
		}
	default:
		m.observeIntegrity(e)
	}
}

// ForgetJob deletes every per-job series of jobID so that deleted jobs do not linger
// in scrapes (and label cardinality stays bounded by the live job set).
func (m *Metrics) ForgetJob(jobID string) {
	if jobID == "" {
		return
	}
	for _, s := range []string{StatusSucceeded, StatusFailed, StatusCancelled} {
		m.backupsTotal.DeleteLabelValues(jobID, s)
	}
	m.backupDuration.DeleteLabelValues(jobID)
	m.backupSize.DeleteLabelValues(jobID)
	byJob := prometheus.Labels{"job": jobID}
	m.databaseBackups.DeletePartialMatch(byJob)
	m.databaseSize.DeletePartialMatch(byJob)
	m.databaseLastSuccess.DeletePartialMatch(byJob)
	m.jobRuns.DeletePartialMatch(byJob)
	m.jobRunDuration.DeletePartialMatch(byJob)
	m.lastSuccessBackup.DeleteLabelValues(jobID)
	m.forgetIntegrityJob(jobID)
	m.forgetRPOJob(jobID)
}

// ObserveNotification counts one notification outcome for a channel type.
func (m *Metrics) ObserveNotification(channelType, outcome string) {
	m.notificationsTotal.WithLabelValues(channelType, outcome).Inc()
}

// ObserveMCPCall counts one MCP tool call by tool and result (ok, error, denied,
// rate_limited). Callers must pass known tool names only (the MCP server maps
// unknown names to "unknown"), so the label cardinality stays bounded.
func (m *Metrics) ObserveMCPCall(tool, result string) {
	m.mcpCalls.WithLabelValues(tool, result).Inc()
}

// IncEventsDropped counts one dropped event; use it as the events.Bus drop hook.
func (m *Metrics) IncEventsDropped(events.Event) {
	m.eventsDropped.Inc()
}
