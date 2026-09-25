package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// ListJobs returns every scheduled job sorted by name.
func (s *Service) ListJobs(ctx context.Context) ([]*models.Job, error) {
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobs, nil
}

// GetJob returns job id or an ErrNotFound error.
func (s *Service) GetJob(ctx context.Context, id string) (*models.Job, error) {
	job, err := s.cfg.Store.GetJob(ctx, id)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	return job, nil
}

// BackupFilter narrows ListBackups. Zero fields match everything.
type BackupFilter struct {
	// Database keeps backups of this database.
	Database string
	// ConnectionID keeps backups taken from this connection.
	ConnectionID string
	// Status keeps backups in this state.
	Status models.BackupStatus
}

// ListBackups returns the backup records matching f, newest first.
func (s *Service) ListBackups(ctx context.Context, f BackupFilter) ([]*models.BackupRecord, error) {
	list, err := s.cfg.Store.ListBackupRecords(ctx, f.Database)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	if f.ConnectionID == "" && f.Status == "" {
		return list, nil
	}
	return slices.DeleteFunc(list, func(b *models.BackupRecord) bool {
		return (f.ConnectionID != "" && b.ConnectionID != f.ConnectionID) || (f.Status != "" && b.Status != f.Status)
	}), nil
}

// GetBackup returns backup record id or an ErrNotFound error.
func (s *Service) GetBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	b, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	return b, nil
}

// ListRestores returns every restore record, newest first.
func (s *Service) ListRestores(ctx context.Context) ([]*models.RestoreRecord, error) {
	list, err := s.cfg.Store.ListRestoreRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	return list, nil
}

// GetRestore returns restore record id or an ErrNotFound error.
func (s *Service) GetRestore(ctx context.Context, id string) (*models.RestoreRecord, error) {
	r, err := s.cfg.Store.GetRestoreRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "restore not found")
	}
	return r, nil
}

// notFound maps store.ErrNotFound to an ErrNotFound error with message msg.
func notFound(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return public(msg, ErrNotFound, err)
	}
	return err
}

// TargetRef names a storage target.
type TargetRef struct {
	// ID is the target ID.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Type is "local" or "s3".
	Type models.StorageType `json:"type"`
}

// Stats are the dashboard KPIs.
type Stats struct {
	// TotalBackups counts every backup record.
	TotalBackups int `json:"total_backups"`
	// CompletedBackups counts completed backups.
	CompletedBackups int `json:"completed_backups"`
	// FailedBackups counts failed backups.
	FailedBackups int `json:"failed_backups"`
	// TotalBytes sums the size of completed backups.
	TotalBytes int64 `json:"total_bytes"`
	// ActiveJobs counts enabled jobs.
	ActiveJobs int `json:"active_jobs"`
	// StorageType is the type of the default storage target, when there is one.
	StorageType models.StorageType `json:"storage_type,omitempty"`
	// DefaultStorageTarget is the default storage target, when there is one.
	DefaultStorageTarget *TargetRef `json:"default_storage_target,omitempty"`
}

// Stats computes the dashboard KPIs. Records that cannot be read count as zero, so the
// dashboard keeps working while the store is degraded.
func (s *Service) Stats(ctx context.Context) Stats {
	backups, _ := s.cfg.Store.ListBackupRecords(ctx, "")
	jobs, _ := s.cfg.Store.ListJobs(ctx)
	return s.stats(ctx, backups, jobs)
}

func (s *Service) stats(ctx context.Context, backups []*models.BackupRecord, jobs []*models.Job) Stats {
	var st Stats
	for _, j := range jobs {
		if j.Enabled {
			st.ActiveJobs++
		}
	}
	st.TotalBackups = len(backups)
	for _, b := range backups {
		switch b.Status {
		case models.StatusCompleted:
			st.CompletedBackups++
			st.TotalBytes += b.SizeBytes
		case models.StatusFailed:
			st.FailedBackups++
		}
	}
	if s.cfg.Targets != nil {
		if def, err := s.cfg.Targets.Resolve(ctx, ""); err == nil {
			st.StorageType = def.Type
			st.DefaultStorageTarget = &TargetRef{ID: def.ID, Name: def.Name, Type: def.Type}
		}
	}
	return st
}

// JobStatus summarises one scheduled job for Status.
type JobStatus struct {
	// ID is the job ID.
	ID string `json:"id"`
	// Name is the job name.
	Name string `json:"name"`
	// Database is the backed-up database.
	Database string `json:"database"`
	// Enabled reports whether the schedule is active.
	Enabled bool `json:"enabled"`
	// LastSuccessAt is when the newest completed backup of the job finished.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// LastSuccessBackupID is that backup's ID.
	LastSuccessBackupID string `json:"last_success_backup_id,omitempty"`
	// NextRun is the next scheduled run.
	NextRun *time.Time `json:"next_run,omitempty"`
}

// FailedBackup summarises a failed backup for Status.
type FailedBackup struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// Database is the database.
	Database string `json:"database"`
	// JobID is the job that ran it, if any.
	JobID string `json:"job_id,omitempty"`
	// StartedAt is when it started.
	StartedAt time.Time `json:"started_at"`
	// Error is the (redacted) failure reason.
	Error string `json:"error,omitempty"`
}

// Status is an operational overview for assistants and monitoring.
type Status struct {
	// Health is "healthy" while the service can read its metadata.
	Health string `json:"health"`
	// Version is the build version.
	Version string `json:"version"`
	// Time is the server time.
	Time time.Time `json:"time"`
	// Stats are the dashboard KPIs.
	Stats Stats `json:"stats"`
	// Connections counts managed connections.
	Connections int `json:"connections"`
	// Jobs counts scheduled jobs.
	Jobs int `json:"jobs"`
	// RunningBackups counts backups in progress.
	RunningBackups int `json:"running_backups"`
	// RunningRestores counts restores in progress.
	RunningRestores int `json:"running_restores"`
	// JobStatus lists every job with its last successful backup.
	JobStatus []JobStatus `json:"job_status"`
	// FailedLast24h lists backups that failed in the last 24 hours, newest first.
	FailedLast24h []FailedBackup `json:"failed_last_24h"`
	// FailedLast24hTruncated reports that FailedLast24h was cut to MaxStatusFailures.
	FailedLast24hTruncated bool `json:"failed_last_24h_truncated,omitempty"`
}

// MaxStatusFailures caps Status.FailedLast24h.
const MaxStatusFailures = 20

// Status returns an operational overview: health, version, counts, the last
// successful backup of every job and the backups that failed in the last 24 hours.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	backups, err := s.cfg.Store.ListBackupRecords(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	restores, err := s.cfg.Store.ListRestoreRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	now := s.now().UTC()
	st := &Status{
		Health: "healthy", Version: s.cfg.Version, Time: now,
		Stats: s.stats(ctx, backups, jobs), Jobs: len(jobs),
		JobStatus: make([]JobStatus, 0, len(jobs)), FailedLast24h: []FailedBackup{},
	}
	if s.cfg.Connections != nil {
		if conns, cerr := s.cfg.Connections.List(ctx); cerr == nil {
			st.Connections = len(conns)
		}
	}
	for _, r := range restores {
		if r.Status == models.RestoreStatusInProgress {
			st.RunningRestores++
		}
	}
	// Backups are newest first: the first completed one per job is its last success.
	lastSuccess := make(map[string]*models.BackupRecord, len(jobs))
	since := now.Add(-24 * time.Hour)
	for _, b := range backups {
		switch b.Status {
		case models.StatusInProgress:
			st.RunningBackups++
		case models.StatusCompleted:
			if _, seen := lastSuccess[b.JobID]; b.JobID != "" && !seen {
				lastSuccess[b.JobID] = b
			}
		case models.StatusFailed:
			if b.StartedAt.Before(since) {
				continue
			}
			if len(st.FailedLast24h) == MaxStatusFailures {
				st.FailedLast24hTruncated = true
				continue
			}
			st.FailedLast24h = append(st.FailedLast24h, FailedBackup{
				ID: b.ID, Database: b.Database, JobID: b.JobID, StartedAt: b.StartedAt, Error: b.ErrorMessage,
			})
		}
	}
	for _, j := range jobs {
		js := JobStatus{ID: j.ID, Name: j.Name, Database: j.Database, Enabled: j.Enabled, NextRun: j.NextRun}
		if b := lastSuccess[j.ID]; b != nil {
			at := b.StartedAt
			if b.CompletedAt != nil {
				at = *b.CompletedAt
			}
			js.LastSuccessAt, js.LastSuccessBackupID = &at, b.ID
		}
		st.JobStatus = append(st.JobStatus, js)
	}
	return st, nil
}
