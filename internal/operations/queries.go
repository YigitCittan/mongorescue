package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
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
		models.StatusPending, models.StatusInProgress, models.StatusCompleted, models.StatusFailed, models.StatusCancelled, models.StatusPruned, models.StatusMissing,
	}
	validTriggers = []models.BackupTrigger{
		models.TriggerScheduled, models.TriggerOnDemand, models.TriggerManual, models.TriggerMCP,
	}
	validRestoreStatuses = []models.RestoreStatus{
		models.RestoreStatusPending, models.RestoreStatusInProgress, models.RestoreStatusCompleted, models.RestoreStatusFailed, models.RestoreStatusCancelled,
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
		"database": f.Database, "connection_id": f.ConnectionID, "job_id": f.JobID, "retry_of": f.RetryOf, "run_id": f.RunID, "q": f.Search,
	}); err != nil {
		return nil, err
	}
	if len(f.IDs) > store.MaxFilterIDs {
		return nil, public(fmt.Sprintf("id takes at most %d IDs", store.MaxFilterIDs), ErrInvalid)
	}
	for _, id := range f.IDs {
		if err := checkText(map[string]string{"id": id}); err != nil {
			return nil, err
		}
	}
	page, err := s.cfg.Store.QueryBackupRecords(ctx, f)
	if err != nil {
		return nil, filterError(err, "backups")
	}
	out := &BackupPage{Items: make([]BackupItem, 0, len(page.Rows)), Total: page.Total}
	records := make([]*models.BackupRecord, 0, len(page.Rows))
	for _, row := range page.Rows {
		records = append(records, row.Record)
		out.Items = append(out.Items, BackupItem{BackupRecord: row.Record, RetriedBy: row.RetriedBy})
	}
	s.withBackupProgress(records)
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
	s.withBackupProgress([]*models.BackupRecord{b})
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
	s.withRestoreProgress(page.Records)
	return &RestorePage{Items: page.Records, Total: page.Total}, nil
}

// ListRestores returns every restore record, newest first.
func (s *Service) ListRestores(ctx context.Context) ([]*models.RestoreRecord, error) {
	list, err := s.cfg.Store.ListRestoreRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	s.withRestoreProgress(list)
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
	s.withRestoreProgress([]*models.RestoreRecord{r})
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

// JobRunBrief summarises a job run for the dashboard KPIs.
type JobRunBrief struct {
	// ID is the run ID.
	ID string `json:"id"`
	// Status is the run's outcome (running while it runs).
	Status models.JobRunStatus `json:"status"`
	// StartedAt is when it started.
	StartedAt time.Time `json:"started_at"`
	// Databases and Succeeded count its databases and those backed up.
	Databases int `json:"databases"`
	Succeeded int `json:"succeeded"`
	// FailedDatabases names the databases that failed.
	FailedDatabases []string `json:"failed_databases,omitempty"`
}

// briefRun summarises run.
func briefRun(run *models.JobRun) JobRunBrief {
	ok, _, _, _ := run.Counts()
	return JobRunBrief{ID: run.ID, Status: run.Status, StartedAt: run.StartedAt, Databases: len(run.Databases), Succeeded: ok,
		FailedDatabases: run.FailedDatabases()}
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
	// FailedBackups counts failed backups (cancelled ones are not failures).
	FailedBackups int `json:"failed_backups"`
	// FailedBackups24h counts backups that started in the last 24 hours and failed.
	FailedBackups24h int `json:"failed_backups_24h"`
	// CancelledBackups counts cancelled backups.
	CancelledBackups int `json:"cancelled_backups"`
	// TotalBytes sums the size of completed backups.
	TotalBytes int64 `json:"total_bytes"`
	// ActiveBackups counts backups that are pending or in progress.
	ActiveBackups int `json:"active_backups"`
	// TotalRestores counts every restore record.
	TotalRestores int `json:"total_restores"`
	// ActiveRestores counts restores that are pending or in progress.
	ActiveRestores int `json:"active_restores"`
	// ActiveJobs counts enabled jobs.
	ActiveJobs int `json:"active_jobs"`
	// LastBackup is the newest backup, when there is one.
	LastBackup *BackupBrief `json:"last_backup,omitempty"`
	// JobLastBackups maps each job ID to its newest backup (without error message).
	JobLastBackups map[string]BackupBrief `json:"job_last_backups"`
	// JobLastRuns maps each multi-database job ID to its newest run: a job's last
	// outcome is its run's (ok, partial, failed), never one database's backup.
	JobLastRuns map[string]JobRunBrief `json:"job_last_runs"`
	// StorageType is the type of the default storage target, when there is one.
	StorageType models.StorageType `json:"storage_type,omitempty"`
	// DefaultStorageTarget is the default storage target, when there is one.
	DefaultStorageTarget *TargetRef `json:"default_storage_target,omitempty"`
	// Degraded reports that some figures could not be read and show as zero.
	Degraded bool `json:"degraded,omitempty"`
	// DegradedReason says what could not be read, when Degraded.
	DegradedReason string `json:"degraded_reason,omitempty"`
	// CorruptRecords lists the stored rows that cannot be read (see
	// store.CorruptRecord). Stats leaves it empty; the REST API fills it for
	// administrators.
	CorruptRecords []store.CorruptRecord `json:"corrupt_records,omitempty"`
}

// Stats computes the dashboard KPIs from SQL aggregates, never reading every record.
// Aggregates that cannot be read count as zero, so the dashboard keeps working while
// the store is degraded; Degraded and DegradedReason then say so, and the failure is
// logged.
func (s *Service) Stats(ctx context.Context) Stats {
	var reasons []string
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		reasons = append(reasons, "list jobs: "+err.Error())
	}
	st, _, _, err := s.stats(ctx, jobs, s.now())
	if err != nil {
		reasons = append(reasons, err.Error())
	}
	if len(reasons) > 0 {
		st.Degraded, st.DegradedReason = true, redact.Text(strings.Join(reasons, "; "))
		s.logger.Warn("dashboard statistics are incomplete", slog.String("reason", st.DegradedReason))
	}
	return st
}

// CorruptRecords returns the stored rows that cannot be read and are skipped by every
// list (see store.CorruptRecord). The rows are never changed.
func (s *Service) CorruptRecords(ctx context.Context) ([]store.CorruptRecord, error) {
	list, err := s.cfg.Store.CorruptRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("check stored records: %w", err)
	}
	return list, nil
}

// stats computes the KPIs at now, and returns the aggregates they were computed from.
// On an error it returns what it has read so far.
func (s *Service) stats(ctx context.Context, jobs []*models.Job, now time.Time) (Stats, *store.BackupStats, *store.RestoreStats, error) {
	st := Stats{JobLastBackups: map[string]BackupBrief{}, JobLastRuns: map[string]JobRunBrief{}}
	var multi []string
	for _, j := range jobs {
		if j.Enabled {
			st.ActiveJobs++
		}
		if j.MultiDatabase() {
			multi = append(multi, j.ID)
		}
	}
	// The newest run of every multi-database job in one query (the dashboard polls
	// this). A failed read leaves the map empty, as each failed lookup used to.
	if len(multi) > 0 {
		if runs, err := s.cfg.Store.LatestJobRuns(ctx, multi); err == nil {
			for id, run := range runs {
				st.JobLastRuns[id] = briefRun(run)
			}
		}
	}
	if s.cfg.Targets != nil {
		if def, err := s.cfg.Targets.Resolve(ctx, ""); err == nil {
			st.StorageType = def.Type
			st.DefaultStorageTarget = &TargetRef{ID: def.ID, Name: def.Name, Type: def.Type}
		}
	}
	bs, err := s.cfg.Store.BackupStats(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return st, nil, nil, fmt.Errorf("backup stats: %w", err)
	}
	st.TotalBackups, st.TotalBytes, st.FailedBackups24h = bs.Total, bs.CompletedBytes, bs.FailedSince
	st.CompletedBackups, st.FailedBackups = bs.ByStatus[models.StatusCompleted], bs.ByStatus[models.StatusFailed]
	st.CancelledBackups = bs.ByStatus[models.StatusCancelled]
	st.ActiveBackups = bs.Active()
	if b := bs.Last; b != nil {
		st.LastBackup = &BackupBrief{ID: b.ID, Database: b.Database, JobID: b.JobID, Status: b.Status, StartedAt: b.StartedAt, ErrorMessage: b.ErrorMessage}
	}
	latest, err := s.cfg.Store.LatestJobBackups(ctx, "")
	if err != nil {
		return st, bs, nil, fmt.Errorf("latest job backups: %w", err)
	}
	for id, b := range latest {
		st.JobLastBackups[id] = BackupBrief{ID: b.ID, Database: b.Database, JobID: b.JobID, Status: b.Status, StartedAt: b.StartedAt}
	}
	rs, err := s.cfg.Store.RestoreStats(ctx)
	if err != nil {
		return st, bs, nil, fmt.Errorf("restore stats: %w", err)
	}
	st.TotalRestores, st.ActiveRestores = rs.Total, rs.Active()
	return st, bs, rs, nil
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
	// LastSuccessAt is when the newest completed backup of the job finished. For a
	// job with several databases it is the oldest of its databases' newest successes
	// (the stalest database), and empty while one of them never succeeded, so a
	// database that fails every day shows.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// LastSuccessBackupID is that backup's ID.
	LastSuccessBackupID string `json:"last_success_backup_id,omitempty"`
	// StalestDatabase names the database LastSuccessAt belongs to (multi-database
	// jobs).
	StalestDatabase string `json:"stalest_database,omitempty"`
	// Databases breaks the last successes down per database (multi-database jobs).
	Databases []DatabaseStatus `json:"databases,omitempty"`
	// NextRun is the next scheduled run.
	NextRun *time.Time `json:"next_run,omitempty"`
}

// DatabaseStatus is the last successful backup of one database of a job.
type DatabaseStatus struct {
	// Database is the database.
	Database string `json:"database"`
	// LastSuccessAt is when its newest completed backup finished; empty if it never
	// succeeded.
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// LastSuccessBackupID is that backup's ID.
	LastSuccessBackupID string `json:"last_success_backup_id,omitempty"`
}

// finishedAt is when b finished (its start for records without a completion time).
func finishedAt(b *models.BackupRecord) time.Time {
	if b.CompletedAt != nil {
		return *b.CompletedAt
	}
	return b.StartedAt
}

// jobDatabases returns the databases a multi-database job is expected to back up:
// the listed ones, else its known and named ones, else those it has backups of.
func jobDatabases(j *models.Job, latest map[string]*models.BackupRecord) []string {
	sel := j.Selection()
	names := slices.Clone(sel.Databases)
	if sel.Mode != models.SelectionList {
		names = append(names, j.KnownDatabases...)
		if j.KnownDatabases == nil {
			for db := range latest {
				names = append(names, db)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// multiJobStatus fills the per-database last successes of multi-database job j
// into js: LastSuccessAt is the stalest database's.
func (s *Service) multiJobStatus(ctx context.Context, j *models.Job, js *JobStatus) error {
	latest, err := s.cfg.Store.LatestJobDatabaseBackups(ctx, j.ID, models.StatusCompleted)
	if err != nil {
		return fmt.Errorf("latest backups of job %s: %w", j.ID, err)
	}
	js.LastSuccessAt, js.LastSuccessBackupID = nil, ""
	never := false
	for _, db := range jobDatabases(j, latest) {
		ds := DatabaseStatus{Database: db}
		b := latest[db]
		if b == nil {
			if !never {
				never = true
				js.StalestDatabase = db
			}
			js.Databases = append(js.Databases, ds)
			continue
		}
		at := finishedAt(b)
		ds.LastSuccessAt, ds.LastSuccessBackupID = &at, b.ID
		js.Databases = append(js.Databases, ds)
		if !never && (js.LastSuccessAt == nil || at.Before(*js.LastSuccessAt)) {
			js.LastSuccessAt, js.LastSuccessBackupID, js.StalestDatabase = &at, b.ID, db
		}
	}
	if never {
		js.LastSuccessAt, js.LastSuccessBackupID = nil, ""
	}
	return nil
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
	jobs, err := s.cfg.Store.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	now := s.now().UTC()
	kpis, backups, restores, err := s.stats(ctx, jobs, now)
	if err != nil {
		return nil, err
	}
	st := &Status{
		Health: "healthy", Version: s.cfg.Version, Time: now, Stats: kpis, Jobs: len(jobs),
		RunningBackups:  backups.ByStatus[models.StatusInProgress],
		RunningRestores: restores.ByStatus[models.RestoreStatusInProgress],
		JobStatus:       make([]JobStatus, 0, len(jobs)), FailedLast24h: []FailedBackup{},
	}
	if s.cfg.Connections != nil {
		if conns, cerr := s.cfg.Connections.List(ctx); cerr == nil {
			st.Connections = len(conns)
		}
	}
	failed, err := s.cfg.Store.QueryBackupRecords(ctx, store.BackupFilter{
		Status: models.StatusFailed, From: now.Add(-24 * time.Hour), Limit: MaxStatusFailures,
	})
	if err != nil {
		return nil, fmt.Errorf("list failed backups: %w", err)
	}
	st.FailedLast24hTruncated = failed.Total > MaxStatusFailures
	for _, row := range failed.Rows {
		b := row.Record
		st.FailedLast24h = append(st.FailedLast24h, FailedBackup{
			ID: b.ID, Database: b.Database, JobID: b.JobID, StartedAt: b.StartedAt, Error: b.ErrorMessage,
		})
	}
	lastSuccess, err := s.cfg.Store.LatestJobBackups(ctx, models.StatusCompleted)
	if err != nil {
		return nil, fmt.Errorf("latest job backups: %w", err)
	}
	for _, j := range jobs {
		js := JobStatus{ID: j.ID, Name: j.Name, Database: j.Database, Enabled: j.Enabled, NextRun: j.NextRun}
		if b := lastSuccess[j.ID]; b != nil {
			at := finishedAt(b)
			js.LastSuccessAt, js.LastSuccessBackupID = &at, b.ID
		}
		if j.MultiDatabase() {
			if err := s.multiJobStatus(ctx, j, &js); err != nil {
				return nil, err
			}
		}
		st.JobStatus = append(st.JobStatus, js)
	}
	return st, nil
}
