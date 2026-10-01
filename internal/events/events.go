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
	// BackupFailed is emitted when a backup run ends in failure (including an abort
	// by a shutdown, a timeout or a stall).
	BackupFailed EventType = "backup.failed"
	// BackupCancelled is emitted when a backup was cancelled (models.StatusCancelled);
	// it is not a failure.
	BackupCancelled EventType = "backup.cancelled"
	// RestoreSucceeded is emitted when a restore (or dry-run) completes successfully.
	RestoreSucceeded EventType = "restore.succeeded"
	// RestoreFailed is emitted when a restore run ends in failure.
	RestoreFailed EventType = "restore.failed"
	// RestoreCancelled is emitted when a restore was cancelled
	// (models.RestoreStatusCancelled).
	RestoreCancelled EventType = "restore.cancelled"
	// NotificationTest is a synthetic event used by "send test" actions. It is never
	// published on the Bus and cannot be selected by notification rules.
	NotificationTest EventType = "notification.test"
	// EncryptionOffAfterUpgrade is emitted once at startup when the previous
	// (deprecated) configuration had backup encryption enabled but it is off now, so
	// backups since the upgrade are not encrypted. It is a security alert: every
	// enabled channel receives it, whatever the rules (see Broadcast).
	EncryptionOffAfterUpgrade EventType = "security.encryption_off_after_upgrade"
	// BulkCompleted is emitted once per bulk operation (POST /api/v1/{backups,restores,
	// jobs}/bulk that is not a dry run) with its counts in Event.Bulk. The per-item
	// events of the action, if any, are published as for a single item. Notification
	// rules cannot select it.
	BulkCompleted EventType = "bulk.completed"
	// VerificationSucceeded is emitted when a stored archive matched its checksum
	// (after upload, in an integrity sweep or on demand). It feeds metrics only and
	// cannot be selected by notification rules.
	VerificationSucceeded EventType = "verification.succeeded"
	// VerificationFailed is emitted when a stored archive did not match its checksum
	// or could not be read to the end.
	VerificationFailed EventType = "verification.failed"
	// RestoreTestSucceeded is emitted when an automated restore test restored the
	// backup and it matched its manifest.
	RestoreTestSucceeded EventType = "restore_test.succeeded"
	// RestoreTestFailed is emitted when a restore test failed or found differences.
	RestoreTestFailed EventType = "restore_test.failed"
	// DriftDetected is emitted when a storage scan finds orphan archives (objects
	// without a record) or missing ones (records whose object is gone).
	DriftDetected EventType = "storage.drift_detected"
	// RetentionDeleted is emitted for every backup a retention policy deleted.
	RetentionDeleted EventType = "retention.deleted"
)

// Sources of verification events.
const (
	// VerificationAfterUpload is the check right after a backup's upload.
	VerificationAfterUpload = "after_upload"
	// VerificationSweep is the scheduled integrity sweep.
	VerificationSweep = "sweep"
	// VerificationOnDemand is POST /api/v1/backups/{id}/verify (or MCP).
	VerificationOnDemand = "on_demand"
)

// BulkSummary describes a finished bulk operation (see BulkCompleted).
type BulkSummary struct {
	// Resource is "backups", "restores" or "jobs".
	Resource string `json:"resource"`
	// Action is the bulk action, such as "delete".
	Action string `json:"action"`
	// Matched counts the selected items.
	Matched int `json:"matched"`
	// Succeeded, Skipped and Failed count the outcomes.
	Succeeded int `json:"succeeded"`
	// Skipped counts items the action did not apply to (protected, running, missing).
	Skipped int `json:"skipped"`
	// Failed counts items the action failed on.
	Failed int `json:"failed"`
	// Actor names who ran it (user, API key or "system").
	Actor string `json:"actor,omitempty"`
}

// Broadcast reports whether t is delivered to every enabled notification channel
// instead of the channels of matching rules.
func (t EventType) Broadcast() bool {
	return t == EncryptionOffAfterUpgrade
}

// ruleTypes lists the event types that notification rules may subscribe to.
var ruleTypes = []EventType{
	BackupSucceeded, BackupFailed, BackupCancelled, RestoreSucceeded, RestoreFailed, RestoreCancelled,
	VerificationFailed, RestoreTestSucceeded, RestoreTestFailed, DriftDetected, RetentionDeleted,
}

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
	switch t {
	case BackupFailed, RestoreFailed, VerificationFailed, RestoreTestFailed, DriftDetected:
		return true
	default:
		return false
	}
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
	// Verification is the verification outcome (ok, mismatch, error) of
	// verification events, or the outcome of a restore test.
	Verification string `json:"verification,omitempty"`
	// Source says what triggered a verification (after_upload, sweep, on_demand),
	// a restore test (scheduled, manual) or a storage scan (scheduled, manual).
	Source string `json:"source,omitempty"`
	// TargetID and TargetName name the storage target of drift events.
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	// Orphans and Missing count the drift a storage scan found.
	Orphans int `json:"orphans,omitempty"`
	Missing int `json:"missing,omitempty"`
	// Detail is a short redacted explanation (a mismatch, a retention rule).
	Detail string `json:"detail,omitempty"`

	// Bulk summarises a bulk operation (BulkCompleted events only).
	Bulk *BulkSummary `json:"bulk,omitempty"`
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
	switch {
	case rec != nil && rec.Status == models.StatusCancelled:
		e.Type = BackupCancelled
		e.Error = redact.Text(errMsg)
	case runErr != nil || rec == nil || rec.Status != models.StatusCompleted:
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
	if rec != nil && rec.Status == models.RestoreStatusCancelled {
		e.Type = RestoreCancelled
		e.Error = redact.Text(errMsg)
		return e
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
