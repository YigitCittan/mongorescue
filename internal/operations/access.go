package operations

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// accessStore applies the caller's connection access (auth.ConnectionFilter) to the
// store reads of the operations service, so every use case it serves to the REST API
// and MCP (lists, details, logs, runs, retries, verification, restores, bulk
// actions, statistics) sees only the jobs, backups and restores of the connections
// the caller may touch. A record outside them reads as store.ErrNotFound, exactly
// like one that does not exist, so the API answers 404 without leaking it. Writes
// and the optional store interfaces (asserted on Config.Store) are not filtered:
// every use case reads a record through this view before it changes it.
type accessStore struct {
	store.Store
}

// jobVisible reports whether the caller may touch job j.
func jobVisible(ctx context.Context, j *models.Job) bool {
	return auth.ConnectionAllowed(ctx, j.ConnectionID)
}

// backupVisible reports whether the caller may touch backup b.
func backupVisible(ctx context.Context, b *models.BackupRecord) bool {
	return auth.ConnectionAllowed(ctx, b.ConnectionID)
}

// restoreVisible reports whether the caller may touch restore r: both the
// connection its backup came from and the one it restored into.
func restoreVisible(ctx context.Context, r *models.RestoreRecord) bool {
	set := auth.ConnectionFilter(ctx)
	return set.Allows(r.SourceConnectionID) && set.Allows(cmp.Or(r.TargetConnectionID, r.SourceConnectionID))
}

// hidden returns the store's not-found error for a record the caller may not touch.
func hidden(what, id string) error {
	return fmt.Errorf("%w: %s %s", store.ErrNotFound, what, id)
}

// GetJob returns job id, or store.ErrNotFound when the caller may not touch it.
func (a accessStore) GetJob(ctx context.Context, id string) (*models.Job, error) {
	j, err := a.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !jobVisible(ctx, j) {
		return nil, hidden("job", id)
	}
	return j, nil
}

// ListJobs returns the jobs the caller may touch.
func (a accessStore) ListJobs(ctx context.Context) ([]*models.Job, error) {
	list, err := a.Store.ListJobs(ctx)
	if err != nil || !auth.ConnectionFilter(ctx).Limited() {
		return list, err
	}
	out := make([]*models.Job, 0, len(list))
	for _, j := range list {
		if jobVisible(ctx, j) {
			out = append(out, j)
		}
	}
	return out, nil
}

// GetBackupRecord returns backup id, or store.ErrNotFound when the caller may not
// touch it.
func (a accessStore) GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error) {
	b, err := a.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if !backupVisible(ctx, b) {
		return nil, hidden("backup", id)
	}
	return b, nil
}

// GetRestoreRecord returns restore id, or store.ErrNotFound when the caller may not
// touch it.
func (a accessStore) GetRestoreRecord(ctx context.Context, id string) (*models.RestoreRecord, error) {
	r, err := a.Store.GetRestoreRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if !restoreVisible(ctx, r) {
		return nil, hidden("restore", id)
	}
	return r, nil
}

// ListRestoreRecords returns the restores the caller may touch.
func (a accessStore) ListRestoreRecords(ctx context.Context) ([]*models.RestoreRecord, error) {
	list, err := a.Store.ListRestoreRecords(ctx)
	if err != nil || !auth.ConnectionFilter(ctx).Limited() {
		return list, err
	}
	out := make([]*models.RestoreRecord, 0, len(list))
	for _, r := range list {
		if restoreVisible(ctx, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// QueryBackupRecords queries the backups the caller may touch.
func (a accessStore) QueryBackupRecords(ctx context.Context, f store.BackupFilter) (*store.BackupPage, error) {
	f.Connections = f.Connections.Intersect(auth.ConnectionFilter(ctx))
	return a.Store.QueryBackupRecords(ctx, f)
}

// QueryRestoreRecords queries the restores the caller may touch.
func (a accessStore) QueryRestoreRecords(ctx context.Context, f store.RestoreFilter) (*store.RestorePage, error) {
	f.Connections = f.Connections.Intersect(auth.ConnectionFilter(ctx))
	return a.Store.QueryRestoreRecords(ctx, f)
}

// ListBackupDatabases lists the databases of the backups the caller may touch.
func (a accessStore) ListBackupDatabases(ctx context.Context) ([]string, error) {
	if set := auth.ConnectionFilter(ctx); set.Limited() {
		return a.ListBackupDatabasesIn(ctx, set)
	}
	return a.Store.ListBackupDatabases(ctx)
}

// ListRestoreDatabases lists the target databases of the restores the caller may
// touch.
func (a accessStore) ListRestoreDatabases(ctx context.Context) ([]string, error) {
	if set := auth.ConnectionFilter(ctx); set.Limited() {
		return a.ListRestoreDatabasesIn(ctx, set)
	}
	return a.Store.ListRestoreDatabases(ctx)
}

// BackupStats aggregates the backups the caller may touch.
func (a accessStore) BackupStats(ctx context.Context, since time.Time) (*store.BackupStats, error) {
	if set := auth.ConnectionFilter(ctx); set.Limited() {
		return a.BackupStatsIn(ctx, since, set)
	}
	return a.Store.BackupStats(ctx, since)
}

// RestoreStats aggregates the restores the caller may touch.
func (a accessStore) RestoreStats(ctx context.Context) (*store.RestoreStats, error) {
	if set := auth.ConnectionFilter(ctx); set.Limited() {
		return a.RestoreStatsIn(ctx, set)
	}
	return a.Store.RestoreStats(ctx)
}

// BackupHistory aggregates the history of the backups the caller may touch.
func (a accessStore) BackupHistory(ctx context.Context, q store.BackupHistoryQuery) (*store.BackupHistory, error) {
	q.Connections = q.Connections.Intersect(auth.ConnectionFilter(ctx))
	return a.Store.BackupHistory(ctx, q)
}

// LatestJobBackups maps the jobs to their newest backups the caller may touch.
func (a accessStore) LatestJobBackups(ctx context.Context, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	m, err := a.Store.LatestJobBackups(ctx, status)
	if err != nil || !auth.ConnectionFilter(ctx).Limited() {
		return m, err
	}
	maps.DeleteFunc(m, func(_ string, b *models.BackupRecord) bool { return !backupVisible(ctx, b) })
	return m, nil
}

// LatestJobDatabaseBackups maps the databases of job jobID to their newest backups
// the caller may touch.
func (a accessStore) LatestJobDatabaseBackups(ctx context.Context, jobID string, status models.BackupStatus) (map[string]*models.BackupRecord, error) {
	m, err := a.Store.LatestJobDatabaseBackups(ctx, jobID, status)
	if err != nil || !auth.ConnectionFilter(ctx).Limited() {
		return m, err
	}
	maps.DeleteFunc(m, func(_ string, b *models.BackupRecord) bool { return !backupVisible(ctx, b) })
	return m, nil
}

// ListJobRuns lists the runs of job jobID, or answers store.ErrNotFound when the
// caller may not touch the job.
func (a accessStore) ListJobRuns(ctx context.Context, jobID string, limit int) ([]*models.JobRun, error) {
	if auth.ConnectionFilter(ctx).Limited() {
		if _, err := a.GetJob(ctx, jobID); err != nil {
			return nil, err
		}
	}
	return a.Store.ListJobRuns(ctx, jobID, limit)
}
