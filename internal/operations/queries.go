package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// BackupFilter selects, orders and pages backups; zero fields match everything. See
// store.BackupFilter for the fields.
type BackupFilter = store.BackupFilter

// RestoreFilter selects, orders and pages restores; zero fields match everything. See
// store.RestoreFilter for the fields.
type RestoreFilter = store.RestoreFilter

// MaxListLimit is the largest page size of QueryBackups and QueryRestores.
const MaxListLimit = store.MaxListLimit

// maxFilterText bounds the free-text and ID filter values a client may send.
const maxFilterText = 256

// BackupItem is a listed backup record with its newest retry, if any.
type BackupItem struct {
	*models.BackupRecord
	// RetriedBy is the newest backup retrying this one; omitted when there is none.
	RetriedBy *store.RetryRef `json:"retried_by,omitempty"`
}

// BackupPage is one page of QueryBackups.
type BackupPage struct {
	// Items are the backups of the page.
	Items []BackupItem
	// Total counts every backup matching the filter.
	Total int
}

// RestorePage is one page of QueryRestores.
type RestorePage struct {
	// Items are the restores of the page.
	Items []*models.RestoreRecord
	// Total counts every restore matching the filter.
	Total int
}

// validBackupStatuses and the other value sets below are the accepted filter values
// (also listed in the 400 messages).
var (
	validBackupStatuses = []models.BackupStatus{
		models.StatusPending, models.StatusInProgress, models.StatusCompleted, models.StatusFailed, models.StatusPruned,
	}
	validTriggers = []models.BackupTrigger{
		models.TriggerScheduled, models.TriggerOnDemand, models.TriggerManual, models.TriggerMCP,
	}
	validRestoreStatuses = []models.RestoreStatus{
		models.RestoreStatusPending, models.RestoreStatusInProgress, models.RestoreStatusCompleted, models.RestoreStatusFailed,
	}
)

// joinValues lists the accepted values of a filter for an error message.
func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// checkText rejects filter values longer than maxFilterText bytes.
func checkText(fields map[string]string) error {
	for name, v := range fields {
		if len(v) > maxFilterText {
			return public(fmt.Sprintf("%s must be at most %d characters", name, maxFilterText), ErrInvalid)
		}
	}
	return nil
}

// filterError maps store.ErrInvalidFilter to an ErrInvalid error.
func filterError(err error, what string) error {
	if errors.Is(err, store.ErrInvalidFilter) {
		return public(strings.TrimPrefix(err.Error(), "store: invalid list filter: "), ErrInvalid, err)
	}
	return fmt.Errorf("list %s: %w", what, err)
}

// QueryBackups returns the page of backups f selects and the number of all matches.
// Unknown status or trigger values and out-of-range paging return ErrInvalid errors.
func (s *Service) QueryBackups(ctx context.Context, f BackupFilter) (*BackupPage, error) {
	if f.Status != "" && !slices.Contains(validBackupStatuses, f.Status) {
		return nil, public("status must be one of "+joinValues(validBackupStatuses), ErrInvalid)
	}
	if f.Trigger != "" && !slices.Contains(validTriggers, f.Trigger) {
		return nil, public("trigger must be one of "+joinValues(validTriggers), ErrInvalid)
	}
	if err := checkText(map[string]string{
		"database": f.Database, "connection_id": f.ConnectionID, "job_id": f.JobID, "retry_of": f.RetryOf, "q": f.Search,
	}); err != nil {
		return nil, err
	}
	page, err := s.cfg.Store.QueryBackupRecords(ctx, f)
	if err != nil {
		return nil, filterError(err, "backups")
	}
	out := &BackupPage{Items: make([]BackupItem, 0, len(page.Rows)), Total: page.Total}
	for _, row := range page.Rows {
		out.Items = append(out.Items, BackupItem{BackupRecord: row.Record, RetriedBy: row.RetriedBy})
	}
	return out, nil
}

// ListBackups returns every backup record matching f (its Limit and Offset apply),
// without retry information.
func (s *Service) ListBackups(ctx context.Context, f BackupFilter) ([]*models.BackupRecord, error) {
	page, err := s.QueryBackups(ctx, f)
	if err != nil {
		return nil, err
	}
	list := make([]*models.BackupRecord, 0, len(page.Items))
	for _, it := range page.Items {
		list = append(list, it.BackupRecord)
	}
	return list, nil
}

// BackupDatabases returns the distinct database names of all backups, sorted.
func (s *Service) BackupDatabases(ctx context.Context) ([]string, error) {
	dbs, err := s.cfg.Store.ListBackupDatabases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list backup databases: %w", err)
	}
	return dbs, nil
}

// GetBackup returns backup record id or an ErrNotFound error.
func (s *Service) GetBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	b, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	return b, nil
}

// QueryRestores returns the page of restores f selects and the number of all matches.
// Unknown status values and out-of-range paging return ErrInvalid errors.
func (s *Service) QueryRestores(ctx context.Context, f RestoreFilter) (*RestorePage, error) {
	if f.Status != "" && !slices.Contains(validRestoreStatuses, f.Status) {
		return nil, public("status must be one of "+joinValues(validRestoreStatuses), ErrInvalid)
	}
	if err := checkText(map[string]string{"backup_id": f.BackupID, "database": f.TargetDatabase, "q": f.Search}); err != nil {
		return nil, err
	}
	page, err := s.cfg.Store.QueryRestoreRecords(ctx, f)
	if err != nil {
		return nil, filterError(err, "restores")
	}
	return &RestorePage{Items: page.Records, Total: page.Total}, nil
}

// ListRestores returns every restore record, newest first.
func (s *Service) ListRestores(ctx context.Context) ([]*models.RestoreRecord, error) {
	list, err := s.cfg.Store.ListRestoreRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	return list, nil
}

// RestoreDatabases returns the distinct target databases of all restores, sorted.
func (s *Service) RestoreDatabases(ctx context.Context) ([]string, error) {
	dbs, err := s.cfg.Store.ListRestoreDatabases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list restore databases: %w", err)
	}
	return dbs, nil
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

// BackupBrief summarises a backup for the dashboard KPIs.
type BackupBrief struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// Database is the database.
	Database string `json:"database"`
	// JobID is the job that ran it, if any.
	JobID string `json:"job_id,omitempty"`
	// Status is the backup's state.
	Status models.BackupStatus `json:"status"`
	// StartedAt is when it started.
	StartedAt time.Time `json:"started_at"`
	// ErrorMessage is the (redacted) failure reason of a failed backup.
	ErrorMessage string `json:"error_message,omitempty"`
}

// Stats are the dashboard KPIs. They cover every record, independent of any page of
// the backup or restore lists.
type Stats struct {
	// TotalBackups counts every backup record.
	TotalBackups int `json:"total_backups"`
	// CompletedBackups counts completed backups.
	CompletedBackups int `json:"completed_backups"`
	// FailedBackups counts failed backups.
	FailedBackups int `json:"failed_backups"`
	// FailedBackups24h counts backups that started in the last 24 hours and failed.
	FailedBackups24h int `json:"failed_backups_24h"`
	// TotalBytes sums the size of completed backups.
	TotalBytes int64 `json:"total_bytes"`
	// TotalRestores counts every restore record.
	TotalRestores int `json:"total_restores"`
	// ActiveJobs counts enabled jobs.
	ActiveJobs int `json:"active_jobs"`
	// LastBackup is the newest backup, when there is one.
	LastBackup *BackupBrief `json:"last_backup,omitempty"`
	// JobLastBackups maps each job ID to its newest backup (without error message).
	JobLastBackups map[string]BackupBrief `json:"job_last_backups"`
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
	restores := 0
	if page, err := s.cfg.Store.QueryRestoreRecords(ctx, store.RestoreFilter{Limit: 1}); err == nil {
		restores = page.Total
	}
	return s.stats(ctx, backups, jobs, restores)
}

// stats computes the KPIs from backups (newest first), jobs and the restore count.
func (s *Service) stats(ctx context.Context, backups []*models.BackupRecord, jobs []*models.Job, restores int) Stats {
	st := Stats{TotalRestores: restores, JobLastBackups: map[string]BackupBrief{}}
	for _, j := range jobs {
		if j.Enabled {
			st.ActiveJobs++
		}
	}
	st.TotalBackups = len(backups)
	since := s.now().Add(-24 * time.Hour)
	for i, b := range backups {
		brief := BackupBrief{ID: b.ID, Database: b.Database, JobID: b.JobID, Status: b.Status, StartedAt: b.StartedAt}
		if i == 0 {
			last := brief
			last.ErrorMessage = b.ErrorMessage
			st.LastBackup = &last
		}
		if _, seen := st.JobLastBackups[b.JobID]; b.JobID != "" && !seen {
			st.JobLastBackups[b.JobID] = brief
		}
		switch b.Status {
		case models.StatusCompleted:
			st.CompletedBackups++
			st.TotalBytes += b.SizeBytes
		case models.StatusFailed:
			st.FailedBackups++
			if !b.StartedAt.Before(since) {
				st.FailedBackups24h++
			}
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
		Stats: s.stats(ctx, backups, jobs, len(restores)), Jobs: len(jobs),
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
