package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// integritySeries are the series of archive verification, restore tests, storage
// drift and retention. Labels are bounded: verification sources and results and
// restore test results are fixed sets, jobs are the live job set (see ForgetJob)
// and targets the configured storage targets (see ForgetTarget).
type integritySeries struct {
	verifications       *prometheus.CounterVec
	restoreTests        *prometheus.CounterVec
	lastRestoreTestOK   *prometheus.GaugeVec
	orphanArchives      *prometheus.GaugeVec
	missingArchives     *prometheus.GaugeVec
	retentionDeletions  *prometheus.CounterVec
	lastStorageScanTime *prometheus.GaugeVec
}

// newIntegritySeries creates the integrity series and returns them as one collector.
func (m *Metrics) newIntegritySeries() prometheus.Collector {
	m.integrity = integritySeries{
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "verifications_total",
			Help:      "Total number of archive verifications by source (after_upload|sweep|on_demand) and result (ok|mismatch|error).",
		}, []string{"source", "result"}),
		restoreTests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "restore_tests_total",
			Help:      "Total number of automated restore tests by job and result (ok|mismatch|error).",
		}, []string{"job", "result"}),
		lastRestoreTestOK: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_successful_restore_test_timestamp_seconds",
			Help:      "Unix timestamp of the last successful restore test per job.",
		}, []string{"job"}),
		orphanArchives: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "storage_orphan_archives",
			Help:      "Archives found by the last storage scan that have no backup record, per storage target.",
		}, []string{"target"}),
		missingArchives: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "storage_missing_archives",
			Help:      "Backup records whose archive the last storage scan did not find, per storage target.",
		}, []string{"target"}),
		retentionDeletions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "retention_deletions_total",
			Help:      "Total number of backups deleted by retention policies per job.",
		}, []string{"job"}),
		lastStorageScanTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_storage_scan_timestamp_seconds",
			Help:      "Unix timestamp of the last completed storage scan per storage target.",
		}, []string{"target"}),
	}
	for _, source := range []string{events.VerificationAfterUpload, events.VerificationSweep, events.VerificationOnDemand} {
		for _, result := range []models.VerificationStatus{models.VerificationOK, models.VerificationMismatch, models.VerificationError} {
			m.integrity.verifications.WithLabelValues(source, string(result))
		}
	}
	return m.integrity
}

// Describe implements prometheus.Collector.
func (s integritySeries) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range s.collectors() {
		c.Describe(ch)
	}
}

// Collect implements prometheus.Collector.
func (s integritySeries) Collect(ch chan<- prometheus.Metric) {
	for _, c := range s.collectors() {
		c.Collect(ch)
	}
}

func (s integritySeries) collectors() []prometheus.Collector {
	return []prometheus.Collector{s.verifications, s.restoreTests, s.lastRestoreTestOK, s.orphanArchives,
		s.missingArchives, s.retentionDeletions, s.lastStorageScanTime}
}

// observeIntegrity updates the integrity series for e.
func (m *Metrics) observeIntegrity(e events.Event) {
	s := m.integrity
	switch e.Type {
	case events.VerificationSucceeded, events.VerificationFailed:
		if knownVerification(e.Source, e.Verification) {
			s.verifications.WithLabelValues(e.Source, e.Verification).Inc()
		}
	case events.RestoreTestSucceeded, events.RestoreTestFailed:
		job := e.JobID
		if job == "" {
			job = ManualJobLabel
		}
		result := e.Verification
		switch models.RestoreTestStatus(result) {
		case models.RestoreTestOK, models.RestoreTestMismatch:
		default:
			result = string(models.RestoreTestError)
		}
		s.restoreTests.WithLabelValues(job, result).Inc()
		if e.Type == events.RestoreTestSucceeded {
			s.lastRestoreTestOK.WithLabelValues(job).Set(float64(e.Time.UnixNano()) / 1e9)
		}
	case events.RetentionDeleted:
		job := e.JobID
		if job == "" {
			job = ManualJobLabel
		}
		s.retentionDeletions.WithLabelValues(job).Inc()
	}
}

// knownVerification reports whether source and result are in the fixed label sets.
func knownVerification(source, result string) bool {
	switch source {
	case events.VerificationAfterUpload, events.VerificationSweep, events.VerificationOnDemand:
	default:
		return false
	}
	switch models.VerificationStatus(result) {
	case models.VerificationOK, models.VerificationMismatch, models.VerificationError:
		return true
	default:
		return false
	}
}

// ObserveStorageScan records the outcome of a storage scan of target targetID:
// orphans archives without a record and missing records without an archive. The
// gauges are reset by every scan, so resolved drift goes back to zero.
func (m *Metrics) ObserveStorageScan(targetID string, orphans, missing int, at time.Time) {
	if targetID == "" {
		return
	}
	m.integrity.orphanArchives.WithLabelValues(targetID).Set(float64(orphans))
	m.integrity.missingArchives.WithLabelValues(targetID).Set(float64(missing))
	m.integrity.lastStorageScanTime.WithLabelValues(targetID).Set(float64(at.UnixNano()) / 1e9)
}

// ForgetTarget deletes the per-target series of a removed storage target.
func (m *Metrics) ForgetTarget(targetID string) {
	m.integrity.orphanArchives.DeleteLabelValues(targetID)
	m.integrity.missingArchives.DeleteLabelValues(targetID)
	m.integrity.lastStorageScanTime.DeleteLabelValues(targetID)
}

// forgetIntegrityJob deletes the per-job integrity series of jobID.
func (m *Metrics) forgetIntegrityJob(jobID string) {
	for _, r := range []models.RestoreTestStatus{models.RestoreTestOK, models.RestoreTestMismatch, models.RestoreTestError} {
		m.integrity.restoreTests.DeleteLabelValues(jobID, string(r))
	}
	m.integrity.lastRestoreTestOK.DeleteLabelValues(jobID)
	m.integrity.retentionDeletions.DeleteLabelValues(jobID)
}
