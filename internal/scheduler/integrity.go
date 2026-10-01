package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// RetentionLog persists the retention log (implemented by *store.SQLiteStore).
type RetentionLog interface {
	// AppendRetentionLog stores one deletion.
	AppendRetentionLog(ctx context.Context, e *models.RetentionLogEntry) error
}

// Auditor records audit entries (implemented by *audit.Service).
type Auditor interface {
	// Record stores e; it never fails the caller.
	Record(ctx context.Context, e audit.Entry)
}

// AfterBackupFunc runs after a successful scheduled backup of job, once its record
// is persisted and retention applied (the automated restore test hooks in here). It
// runs on the scheduler's run context, so Stop cancels it and waits for it.
type AfterBackupFunc func(ctx context.Context, job *models.Job, record *models.BackupRecord)

// WithRetentionLog records every backup deleted by retention in log.
func WithRetentionLog(log RetentionLog) Option {
	return func(s *Scheduler) { s.retentionLog = log }
}

// WithAuditor records every backup deleted by retention in the audit log.
func WithAuditor(a Auditor) Option {
	return func(s *Scheduler) { s.auditor = a }
}

// WithAfterBackup runs fn after every successful scheduled backup.
func WithAfterBackup(fn AfterBackupFunc) Option {
	return func(s *Scheduler) { s.afterBackup = fn }
}

// AuditToolRetention is the audit log "tool" of retention deletions.
const AuditToolRetention = "retention.delete"

// retentionDeleted records one retention deletion: the retention log, the audit log
// and a retention.deleted event. Failures are logged; retention goes on.
func (s *Scheduler) retentionDeleted(ctx context.Context, e models.RetentionLogEntry) {
	if s.retentionLog != nil {
		entry := e
		if err := s.retentionLog.AppendRetentionLog(context.WithoutCancel(ctx), &entry); err != nil {
			s.logger.Error("failed to record a retention deletion", slog.String("backup_id", e.BackupID), slog.Any("error", err))
		}
	}
	if s.auditor != nil {
		args, _ := json.Marshal(map[string]any{
			"job_id": e.JobID, "backup_id": e.BackupID, "database": e.Database,
			"reason": e.Reason, "detail": e.Detail, "storage_target_id": e.StorageTargetID,
		})
		result := audit.ResultOK
		if e.Error != "" {
			result = audit.ResultError
		}
		s.auditor.Record(ctx, audit.Entry{
			Time: e.Time, APIKeyName: "retention", Transport: audit.TransportSystem,
			Tool: AuditToolRetention, Arguments: args, Result: result, Error: e.Error,
		})
	}
	if s.publisher != nil {
		s.publisher.Publish(ctx, events.RetentionEvent(e))
	}
}
