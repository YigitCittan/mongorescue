package events

import (
	"fmt"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// VerificationEvent builds a verification.succeeded or verification.failed event
// from a backup record that was just verified (source is VerificationAfterUpload,
// VerificationSweep or VerificationOnDemand). ok is false when rec carries no
// verification outcome.
func VerificationEvent(rec *models.BackupRecord, source string) (Event, bool) {
	if rec == nil || rec.Verification == "" {
		return Event{}, false
	}
	e := Event{
		Type:         VerificationSucceeded,
		Time:         time.Now().UTC(),
		JobID:        rec.JobID,
		BackupID:     rec.ID,
		Database:     rec.Database,
		Status:       string(rec.Status),
		SizeBytes:    rec.SizeBytes,
		Verification: string(rec.Verification),
		Source:       source,
	}
	if rec.VerifiedAt != nil {
		e.Time = rec.VerifiedAt.UTC()
	}
	if rec.Verification != models.VerificationOK {
		e.Type = VerificationFailed
		e.Error = failureText(rec.VerificationError, nil)
	}
	return e, true
}

// RestoreTestEvent builds a restore_test.succeeded or restore_test.failed event
// from a finished restore test.
func RestoreTestEvent(r *models.RestoreTestResult) Event {
	e := Event{
		Type:         RestoreTestSucceeded,
		Time:         time.Now().UTC(),
		JobID:        r.JobID,
		BackupID:     r.BackupID,
		Database:     r.Database,
		Status:       string(r.Status),
		Verification: string(r.Status),
		Source:       r.Trigger,
		Duration:     secondsToDuration(r.DurationSeconds),
	}
	if r.CompletedAt != nil {
		e.Time = r.CompletedAt.UTC()
	}
	if r.Status != models.RestoreTestOK {
		e.Type = RestoreTestFailed
		msg := r.Error
		if msg == "" && len(r.Mismatches) > 0 {
			msg = strings.Join(r.Mismatches, "; ")
		}
		e.Error = failureText(msg, nil)
	}
	if r.DropError != "" {
		e.Detail = redact.Text("the temporary database " + r.TempDatabase + " could not be dropped: " + r.DropError)
	}
	return e
}

// DriftEvent builds a storage.drift_detected event for a storage scan of target
// targetID that found orphans orphan archives and missing missing ones.
func DriftEvent(targetID, targetName string, orphans, missing int, source string, at time.Time) Event {
	return Event{
		Type:       DriftDetected,
		Time:       at.UTC(),
		TargetID:   targetID,
		TargetName: targetName,
		Orphans:    orphans,
		Missing:    missing,
		Source:     source,
		Detail:     fmt.Sprintf("%d orphan archive(s), %d missing archive(s)", orphans, missing),
	}
}

// RetentionEvent builds a retention.deleted event for a backup deleted by retention.
func RetentionEvent(e models.RetentionLogEntry) Event {
	return Event{
		Type:      RetentionDeleted,
		Time:      e.Time.UTC(),
		JobID:     e.JobID,
		BackupID:  e.BackupID,
		Database:  e.Database,
		Status:    string(models.StatusPruned),
		SizeBytes: e.SizeBytes,
		Detail:    e.Detail,
		Error:     redact.Text(e.Error),
	}
}
