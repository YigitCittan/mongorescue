package operations

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// DeleteResult is the outcome of deleting one backup. The deletion is soft: the
// archive stays in storage until PurgeAfter, and the backup can be undeleted until
// then (UndeleteBackup).
type DeleteResult struct {
	// DeletedID is the deleted backup record.
	DeletedID string `json:"deleted_id"`
	// Status is models.StatusDeleted.
	Status models.BackupStatus `json:"status"`
	// DeletedAt is when it was deleted.
	DeletedAt time.Time `json:"deleted_at"`
	// PurgeAfter is the end of the grace period: the purge removes the archive
	// afterwards. Until then the deletion can be undone.
	PurgeAfter time.Time `json:"purge_after"`
	// ArchiveDeleted is always false: a deletion frees no storage at once (kept for
	// clients of earlier releases, which deleted the archive here).
	ArchiveDeleted bool `json:"archive_deleted"`
}

// deletionLock takes the deletion lock of rec's job (or database), see
// runs.DeletionKey.
func deletionLock(ctx context.Context, rec *models.BackupRecord) (func(), error) {
	return runs.LockDeletion(ctx, runs.DeletionKey(rec.JobID, rec.ConnectionID, rec.Database))
}

// checkSoftDeletable returns why rec cannot be deleted now, or nil: it is pinned
// (ErrPinned), still running (ErrBackupRunning) or already deleted
// (ErrAlreadyDeleted).
func checkSoftDeletable(rec *models.BackupRecord) error {
	switch {
	case rec.Status.Deleted():
		return public(fmt.Sprintf("backup %s is already %s", rec.ID, rec.Status), ErrAlreadyDeleted)
	case rec.Status == models.StatusPending || rec.Status == models.StatusInProgress:
		return public(fmt.Sprintf("backup %s is still %s; cancel it before deleting it", rec.ID, rec.Status), ErrBackupRunning)
	}
	return CheckDeletable(rec)
}

// DeleteBackup deletes backup id softly, with an optional reason: the record moves to
// models.StatusDeleted with the end of the grace period (security.delete_grace_days)
// as purge_after, and the archive stays in storage until the scheduler's purge
// removes it afterwards; until then UndeleteBackup restores the backup. It holds the
// deletion lock of the backup's job and re-reads the record inside it, so a pin set
// meanwhile is honoured: a pinned backup is refused (ErrPinned), and so is a running
// (ErrBackupRunning) or an already deleted one (ErrAlreadyDeleted). With the
// two-person rule on, the deletion waits for a second administrator
// (*ApprovalPendingError). Expected failures: ErrNotFound, ErrInvalid, ErrPinned,
// ErrBackupRunning, ErrAlreadyDeleted and ErrApprovalRequired.
func (s *Service) DeleteBackup(ctx context.Context, id, reason string) (*DeleteResult, error) {
	reason, err := checkReason(reason)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	if err = checkSoftDeletable(rec); err != nil {
		return nil, err
	}
	if s.needsApproval(ctx) {
		return nil, s.requestApproval(ctx, &models.Approval{Action: models.ApprovalDeleteBackup, Subject: rec.ID, Reason: reason,
			Summary: fmt.Sprintf("delete backup %s (db %s)", rec.ID, rec.Database)})
	}
	unlock, err := deletionLock(ctx, rec)
	if err != nil {
		return nil, err
	}
	defer unlock()
	updated, err := s.softDelete(ctx, rec.ID, reason)
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "delete_backup", fmt.Sprintf("backup %s (db %s) deleted; recoverable until %s", updated.ID, updated.Database, updated.PurgeAfter.Format(time.RFC3339)),
		func(e *events.Event) { e.BackupID, e.JobID, e.Database = updated.ID, updated.JobID, updated.Database })
	return deleteResult(updated), nil
}

// deleteResult describes the soft deletion of rec.
func deleteResult(rec *models.BackupRecord) *DeleteResult {
	res := &DeleteResult{DeletedID: rec.ID, Status: rec.Status}
	if rec.DeletedAt != nil {
		res.DeletedAt = *rec.DeletedAt
	}
	if rec.PurgeAfter != nil {
		res.PurgeAfter = *rec.PurgeAfter
	}
	return res
}

// softDelete marks backup id deleted, re-checking it in the same transaction. The
// caller holds its deletion lock.
func (s *Service) softDelete(ctx context.Context, id, reason string) (*models.BackupRecord, error) {
	now := s.now().UTC()
	by, approvedBy := actorNames(ctx)
	d := models.SoftDelete{At: now, PurgeAfter: now.Add(s.deleteGrace()), By: by, ApprovedBy: approvedBy, Reason: reason}
	mark := func(r *models.BackupRecord) error {
		if err := checkSoftDeletable(r); err != nil {
			return err
		}
		r.MarkDeleted(d)
		return nil
	}
	var updated *models.BackupRecord
	if u, ok := s.cfg.Store.(backupUpdater); ok {
		rec, err := u.UpdateBackupRecord(ctx, id, mark)
		if err != nil {
			return nil, notFound(err, "backup not found (deleted meanwhile)")
		}
		updated = rec
	} else {
		rec, err := s.store.GetBackupRecord(ctx, id)
		if err != nil {
			return nil, notFound(err, "backup not found (deleted meanwhile)")
		}
		if err = mark(rec); err != nil {
			return nil, err
		}
		if err = s.store.SaveBackupRecord(ctx, rec); err != nil {
			return nil, fmt.Errorf("save deleted backup: %w", err)
		}
		updated = rec
	}
	auditlog.Annotate(ctx, "protection", "soft_deleted")
	auditlog.Annotate(ctx, "purge_after", updated.PurgeAfter.Format(time.RFC3339))
	s.logger.With(actorAttrs(ctx)...).Info("backup deleted; its archive is kept until the grace period ends",
		logsafe.Attr("backup_id", updated.ID),
		logsafe.Attr("database", updated.Database),
		slog.Time("purge_after", *updated.PurgeAfter),
	)
	return updated, nil
}

// UndeleteBackup undoes the deletion of backup id during its grace period: the
// record gets back the status it had before (completed, failed, …) and its archive,
// which the deletion kept, is restorable again. It holds the backup's deletion lock,
// so it never interleaves with the purge. A purged backup cannot be undeleted
// (ErrNotDeleted). It needs the admin scope. Expected failures: ErrNotFound,
// ErrNotDeleted and auth.ErrForbidden.
func (s *Service) UndeleteBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("undeleting a backup needs the admin role or an admin API key: %w", err)
	}
	rec, err := s.store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	unlock, err := deletionLock(ctx, rec)
	if err != nil {
		return nil, err
	}
	defer unlock()
	updated, err := s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		switch r.Status {
		case models.StatusDeleted:
			r.Undelete()
			return nil
		case models.StatusPurged:
			return public(fmt.Sprintf("backup %s was purged after its grace period; its archive is gone", r.ID), ErrNotDeleted)
		default:
			return public(fmt.Sprintf("backup %s is not deleted (it is %s)", r.ID, r.Status), ErrNotDeleted)
		}
	})
	if err != nil {
		return nil, err
	}
	auditlog.Annotate(ctx, "protection", "undeleted")
	s.logger.With(actorAttrs(ctx)...).Info("backup undeleted",
		logsafe.Attr("backup_id", updated.ID), slog.String("status", string(updated.Status)))
	return updated, nil
}

// deleteRestore deletes the history record of a restore. The restored database is
// never touched.
func (s *Service) deleteRestore(ctx context.Context, rec *models.RestoreRecord) error {
	if err := s.store.DeleteRestoreRecord(ctx, rec.ID); err != nil {
		return notFound(err, "restore not found (deleted meanwhile)")
	}
	s.logger.With(actorAttrs(ctx)...).Info("restore record deleted",
		logsafe.Attr("restore_id", rec.ID),
		logsafe.Attr("target_database", rec.TargetDatabase),
	)
	return nil
}

// jobUnregisterer removes a job from the running scheduler (implemented by
// *scheduler.Scheduler).
type jobUnregisterer interface {
	UnregisterJob(jobID string)
}

// DeleteJob removes job id from the scheduler and the store; its backups are kept.
// Config.OnJobDeleted is called afterwards. Expected failures: ErrNotFound.
func (s *Service) DeleteJob(ctx context.Context, id string) error {
	if u, ok := s.cfg.Scheduler.(jobUnregisterer); ok {
		u.UnregisterJob(id)
	}
	if err := s.store.DeleteJob(ctx, id); err != nil {
		return notFound(err, "job not found")
	}
	// A pending retention change belongs to the deleted job, never to a job created
	// later under the same ID.
	if st, ok := s.cfg.Store.(pendingStore); ok {
		if _, err := st.DeletePendingChangesOf(ctx, models.PendingRetention, id); err != nil {
			s.logger.Warn("could not drop the pending retention change of a deleted job", logsafe.Attr("job_id", id), logsafe.Error(err))
		}
	}
	if s.cfg.OnJobDeleted != nil {
		s.cfg.OnJobDeleted(id)
	}
	s.logger.With(actorAttrs(ctx)...).Info("job deleted", logsafe.Attr("job_id", id))
	return nil
}

// SetJobEnabled schedules (enabled) or pauses job id and returns the stored job. Only
// the enabled flag changes; the rest is re-read under the scheduler's lock, so a
// concurrent edit is never reverted. Enabling validates the job like UpdateJob (a
// job whose connection is gone cannot be enabled); pausing always works. Expected
// failures: ErrNotFound and those of ValidateJob.
func (s *Service) SetJobEnabled(ctx context.Context, id string, enabled bool) (*models.Job, error) {
	existing, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	if enabled {
		if err = s.ValidateJob(ctx, existing.Clone()); err != nil {
			return nil, err
		}
	}
	job := existing.Clone()
	persist := func() error {
		current, getErr := s.store.GetJob(ctx, id)
		if getErr != nil {
			return notFound(getErr, "job not found")
		}
		*job = *current.Clone()
		job.Enabled = enabled
		job.NextRun = nil
		if enabled && s.cfg.Scheduler == nil {
			job.NextRun = nextRunOf(job.CronExpression, s.now())
		}
		return notFound(s.store.UpdateJob(ctx, job), "job not found")
	}
	if s.cfg.Scheduler != nil {
		err = s.cfg.Scheduler.ApplyJobUpdate(job, persist)
	} else {
		err = persist()
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// actorOf describes the caller in ctx by kind (runs.ActorUser, runs.ActorAPIKey or
// runs.ActorSystem) and user ID ("" for the system and keys without a user). API key
// IDs and names are never logged; the audit entry of a bulk run keeps the key.
func actorOf(ctx context.Context) (kind runs.ActorKind, userID string) {
	p := auth.PrincipalFrom(ctx)
	switch {
	case p == nil:
		return runs.ActorSystem, ""
	case p.Method == auth.MethodAPIKey:
		return runs.ActorAPIKey, p.UserID()
	case p.User != nil:
		return runs.ActorUser, p.UserID()
	default:
		return runs.ActorSystem, ""
	}
}

// actorAttrs are the log attributes of the caller in ctx (see actorOf).
func actorAttrs(ctx context.Context) []any {
	kind, userID := actorOf(ctx)
	return []any{slog.String("actor_kind", string(kind)), logsafe.Attr("actor_user_id", userID)}
}

// actorLabel is the caller in ctx as "<kind>" or "<kind>:<user ID>", for bulk
// summaries.
func actorLabel(ctx context.Context) string {
	kind, userID := actorOf(ctx)
	if userID == "" {
		return string(kind)
	}
	return string(kind) + ":" + userID
}
