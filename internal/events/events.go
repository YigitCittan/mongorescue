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
	"slices"
	"strings"
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
	// JobDatabasesAdded is emitted when a job that includes new databases
	// automatically backs up databases for the first time (Event.Databases).
	JobDatabasesAdded EventType = "job.databases_added"
	// MetadataBackupFailed is emitted when a snapshot of MongoRescue's own metadata
	// database could not be written to its storage target (see internal/metabackup).
	MetadataBackupFailed EventType = "metadata_backup.failed"
	// RestoreVerificationFailed is emitted after restore.succeeded when the restored
	// database did not match the backup's manifest (RestoreRequest.VerifyRestore). The
	// restore stays completed, with a warning; Event.Detail names the first mismatch.
	RestoreVerificationFailed EventType = "restore.verification_failed"
	// JobRPOMissed is emitted once when the newest successful backup of one of a
	// job's databases becomes older than the job's recovery point objective
	// (Event.Database; Event.Detail gives the age and the objective). It is not
	// emitted again for the same breach, also not after a restart.
	JobRPOMissed EventType = "job.rpo_missed"
	// JobRPORecovered is emitted when a database whose RPO breach was reported
	// (JobRPOMissed) has a recent enough successful backup again.
	JobRPORecovered EventType = "job.rpo_recovered"
	// SecurityDestructiveAction is emitted for every destructive action that took
	// effect or was scheduled: backups deleted (single or bulk) or purged, a storage
	// target deleted, a retention shortening or a lower delete grace period
	// scheduled or applied, a backup unpinned, the two-person rule turned off
	// (Event.Action names it, Event.Actor who, Event.Detail what).
	SecurityDestructiveAction EventType = "security.destructive_action"
	// SecurityApprovalRequested is emitted when a destructive action waits for a
	// second administrator (security.require_second_approver; Event.ApprovalID).
	SecurityApprovalRequested EventType = "security.approval_requested"
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
	// Actor is the kind of caller ("user", "api_key" or "system"), with the user ID
	// after a colon when there is one. API key IDs and names are never included.
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
	JobDatabasesAdded, MetadataBackupFailed, RestoreVerificationFailed, JobRPOMissed, JobRPORecovered,
	SecurityDestructiveAction, SecurityApprovalRequested,
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
	case BackupFailed, RestoreFailed, VerificationFailed, RestoreTestFailed, DriftDetected, MetadataBackupFailed,
		RestoreVerificationFailed, JobRPOMissed:
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

	// RunID is the job run a backup event belongs to.
	RunID string `json:"run_id,omitempty"`
	// Run summarises a whole job run. A multi-database run publishes one event per
	// database (InRun) and then one backup event with Run set, which is the one
	// notifications deliver; a single-database run's backup event carries Run too.
	Run *RunSummary `json:"run,omitempty"`
	// InRun marks the per-database backup event of a multi-database run:
	// notifications skip it (the run's summary event follows), metrics count it.
	InRun bool `json:"in_run,omitempty"`
	// Databases names the databases of a JobDatabasesAdded event.
	Databases []string `json:"databases,omitempty"`

	// Action names the destructive action of security events (such as
	// "delete_backup" or "purge").
	Action string `json:"action,omitempty"`
	// Actor names who took it: a username, "API key <name>", "retention" or "system".
	Actor string `json:"actor,omitempty"`
	// ApprovalID is the approval request of security events, if any.
	ApprovalID string `json:"approval_id,omitempty"`
}

// SecurityEvent returns a SecurityDestructiveAction event (or, with approvalID and
// requested, a SecurityApprovalRequested one) for action by actor; detail is
// redacted.
func SecurityEvent(t EventType, at time.Time, action, actor, approvalID, detail string) Event {
	return Event{Type: t, Time: at.UTC(), Action: action, Actor: actor, ApprovalID: approvalID, Detail: redact.Text(detail)}
}

// RunSummary describes a finished job run (see Event.Run).
type RunSummary struct {
	// Status is the run's status: ok, partial, failed or cancelled.
	Status string `json:"status"`
	// Multi reports a run of a multi-database job.
	Multi bool `json:"multi,omitempty"`
	// Databases counts the run's databases.
	Databases int `json:"databases"`
	// Succeeded, Failed and Cancelled count their outcomes.
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// FailedDatabases names the databases that failed.
	FailedDatabases []string `json:"failed_databases,omitempty"`
	// SkippedDatabases names the databases skipped because another backup of them
	// was already running; they are neither succeeded nor failed.
	SkippedDatabases []string `json:"skipped_databases,omitempty"`
	// NewDatabases names databases found since the last run that were not backed up.
	NewDatabases []string `json:"new_databases,omitempty"`
}

// RunSummaryOf summarises run.
func RunSummaryOf(run *models.JobRun, multi bool) *RunSummary {
	if run == nil {
		return nil
	}
	ok, failed, cancelled, _ := run.Counts()
	return &RunSummary{
		Status: string(run.Status), Multi: multi, Databases: len(run.Databases),
		Succeeded: ok, Failed: failed, Cancelled: cancelled,
		FailedDatabases: run.FailedDatabases(), SkippedDatabases: run.SkippedDatabases(), NewDatabases: slices.Clone(run.NewDatabases),
	}
}

// JobRunEvent builds the summary event of a finished multi-database job run: a
// backup.succeeded event when every database succeeded, backup.cancelled when the
// run was cancelled without failures, and backup.failed otherwise (also for a
// partial run, whose Status is "partial"). Error names the failed databases.
func JobRunEvent(run *models.JobRun) Event {
	e := Event{Type: BackupSucceeded, Time: time.Now().UTC(), JobID: run.JobID, RunID: run.ID, Status: string(run.Status)}
	if run.CompletedAt != nil {
		e.Time = run.CompletedAt.UTC()
	}
	e.Duration = secondsToDuration(run.DurationSeconds)
	e.Run = RunSummaryOf(run, true)
	switch run.Status {
	case models.JobRunOK:
	case models.JobRunCancelled:
		e.Type = BackupCancelled
	default:
		e.Type = BackupFailed
		var parts []string
		for _, d := range run.Databases {
			switch d.Status {
			case models.StatusCompleted, models.StatusCancelled, models.StatusInProgress, models.StatusPending, models.StatusSkipped:
			default:
				msg := d.Database
				if d.Error != "" {
					msg += ": " + d.Error
				}
				parts = append(parts, msg)
			}
		}
		if run.Error != "" {
			parts = append([]string{run.Error}, parts...)
		}
		e.Error = redact.Text(strings.Join(parts, "; "))
		if e.Error == "" {
			e.Error = "no database was backed up"
		}
	}
	return e
}

// DatabasesAddedEvent builds the JobDatabasesAdded event of a job run that backed
// up databases for the first time.
func DatabasesAddedEvent(jobID, runID string, databases []string) Event {
	return Event{
		Type: JobDatabasesAdded, Time: time.Now().UTC(), JobID: jobID, RunID: runID,
		Databases: slices.Clone(databases), Detail: strings.Join(databases, ", "),
	}
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
