package metrics

import "time"

// BackupSeed is the newest successful backup of one database of a job, as read from
// the metadata store.
type BackupSeed struct {
	// JobID is the job that took the backup.
	JobID string
	// Database is the backed up database; empty for a record without one.
	Database string
	// At is when the backup completed (the time its BackupSucceeded event carried).
	At time.Time
	// SizeBytes is the stored archive size.
	SizeBytes int64
}

// SeedLastBackups sets the last-successful-backup timestamp and size gauges (per job
// and per job and database) from the newest successful backup of every database, so
// they are exposed after a restart or an HA takeover and not only after the next
// BackupSucceeded event. It follows ObserveEvent: the per-database series come from
// each seed, and the job series from the newest seed of the job (the last database
// of a multi-database run). An empty JobID is ignored. Call it before the instance
// subscribes to the event bus, with only the jobs that still exist.
func (m *Metrics) SeedLastBackups(seeds []BackupSeed) {
	newest := map[string]BackupSeed{}
	for _, s := range seeds {
		if s.JobID == "" {
			continue
		}
		if s.Database != "" {
			m.databaseSize.WithLabelValues(s.JobID, s.Database).Set(float64(s.SizeBytes))
			m.databaseLastSuccess.WithLabelValues(s.JobID, s.Database).Set(float64(s.At.UnixNano()) / 1e9)
		}
		if cur, ok := newest[s.JobID]; !ok || s.At.After(cur.At) {
			newest[s.JobID] = s
		}
	}
	for job, s := range newest {
		m.backupSize.WithLabelValues(job).Set(float64(s.SizeBytes))
		m.lastSuccessBackup.WithLabelValues(job).Set(float64(s.At.UnixNano()) / 1e9)
	}
}
