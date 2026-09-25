// Package events defines the domain events emitted when backups and restores finish,
// and an in-process, non-blocking event Bus that fans them out to subscribers such as
// the notification dispatcher and the Prometheus metrics collector.
//
// Publishers (the scheduler and the HTTP delivery layer) never block and never fail
// because of a slow or broken subscriber: events are buffered in a bounded queue and
// dropped (with a log line and a drop hook) when the queue is full.
package events

import (
	"context"
	"errors"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// EventType identifies the kind of domain event.
type EventType string

const (
	// BackupSucceeded is emitted when a backup completes and is persisted to storage.
	BackupSucceeded EventType = "backup.succeeded"
	// BackupFailed is emitted when a backup run ends in failure (including cancellation).
	BackupFailed EventType = "backup.failed"
	// RestoreSucceeded is emitted when a restore (or dry-run) completes successfully.
	RestoreSucceeded EventType = "restore.succeeded"
	// RestoreFailed is emitted when a restore run ends in failure.
	RestoreFailed EventType = "restore.failed"
	// NotificationTest is a synthetic event used by "send test" actions. It is never
	// published on the Bus and cannot be selected by notification rules.
	NotificationTest EventType = "notification.test"
)

// ruleTypes lists the event types that notification rules may subscribe to.
var ruleTypes = []EventType{BackupSucceeded, BackupFailed, RestoreSucceeded, RestoreFailed}

// RuleTypes returns the event types that notification rules may subscribe to, in a
// stable display order. The returned slice is a fresh copy.
func RuleTypes() []EventType {
	out := make([]EventType, len(ruleTypes))
	copy(out, ruleTypes)
	return out
}

// Subscribable reports whether t can be selected by a notification rule.
func (t EventType) Subscribable() bool {
	for _, rt := range ruleTypes {
		if t == rt {
			return true
		}
	}
	return false
}

// Failed reports whether t describes a failed operation.
func (t EventType) Failed() bool {
	return t == BackupFailed || t == RestoreFailed
}

// Event is an immutable description of a finished backup or restore operation.
// Error is always redacted (connection-string credentials masked) before it is set.
type Event struct {
	// Type identifies the kind of event.
	Type EventType `json:"type"`
	// Time is when the operation finished (UTC).
	Time time.Time `json:"time"`
	// JobID is the scheduled job that produced a backup event; empty for manual runs.
	JobID string `json:"job_id,omitempty"`
	// BackupID is the backup record (for restores: the source backup).
	BackupID string `json:"backup_id,omitempty"`
	// RestoreID is the restore record for restore events.
	RestoreID string `json:"restore_id,omitempty"`
	// Database is the backed-up database, or the restore target database.
	Database string `json:"database,omitempty"`
	// Status is the final record status (e.g. "completed", "failed").
	Status string `json:"status,omitempty"`
	// Error is the redacted failure reason; empty on success.
	Error string `json:"error,omitempty"`
	// Duration is the execution time of the operation.
	Duration time.Duration `json:"duration"`
	// SizeBytes is the archive size for backup events.
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// Publisher is the port through which business and delivery layers emit events.
// Implementations must never block the caller.
type Publisher interface {
	// Publish enqueues e for asynchronous delivery and reports whether it was accepted.
	Publish(ctx context.Context, e Event) bool
}

// BackupEvent builds a backup.succeeded or backup.failed event from the outcome of a
// backup engine run. rec may be nil when the engine failed before creating a record, in
// which case jobID and database identify the run. runErr is the error returned by the
// engine; the event is a failure when runErr is non-nil or the record is not completed.
func BackupEvent(rec *models.BackupRecord, runErr error, jobID, database string) Event {
	e := Event{
		Type:     BackupSucceeded,
		Time:     time.Now().UTC(),
		JobID:    jobID,
		Database: database,
	}
	var errMsg string
	if rec != nil {
		e.BackupID = rec.ID
		if rec.Database != "" {
			e.Database = rec.Database
		}
		e.Status = string(rec.Status)
		e.SizeBytes = rec.SizeBytes
		e.Duration = secondsToDuration(rec.DurationSeconds)
		if rec.CompletedAt != nil {
			e.Time = rec.CompletedAt.UTC()
		}
		errMsg = rec.ErrorMessage
	}
	if runErr != nil || rec == nil || rec.Status != models.StatusCompleted {
		e.Type = BackupFailed
		if e.Status == "" {
			e.Status = string(models.StatusFailed)
		}
		e.Error = failureText(errMsg, runErr)
	}
	return e
}

// RestoreEvent builds a restore.succeeded or restore.failed event from the outcome of a
// restore engine run. rec may be nil when the engine failed before creating a record.
func RestoreEvent(rec *models.RestoreRecord, runErr error, backupID string) Event {
	e := Event{
		Type:     RestoreSucceeded,
		Time:     time.Now().UTC(),
		BackupID: backupID,
	}
	var errMsg string
	if rec != nil {
		e.RestoreID = rec.ID
		if rec.BackupID != "" {
			e.BackupID = rec.BackupID
		}
		e.Database = rec.TargetDatabase
		e.Status = string(rec.Status)
		e.Duration = secondsToDuration(rec.DurationSeconds)
		if rec.CompletedAt != nil {
			e.Time = rec.CompletedAt.UTC()
		}
		errMsg = rec.ErrorMessage
	}
	if runErr != nil || rec == nil || rec.Status != models.RestoreStatusCompleted {
		e.Type = RestoreFailed
		if e.Status == "" {
			e.Status = string(models.RestoreStatusFailed)
		}
		e.Error = failureText(errMsg, runErr)
	}
	return e
}

// failureText picks the most descriptive failure reason and redacts it.
func failureText(recordMsg string, runErr error) string {
	msg := recordMsg
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	if errors.Is(runErr, context.Canceled) && recordMsg == "" {
		msg = "operation cancelled"
	}
	return redact.Text(msg)
}

// secondsToDuration converts fractional seconds into a time.Duration.
func secondsToDuration(s float64) time.Duration {
	if s <= 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}
