package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// CopyKeeps says which backups the per copy target retention (models.Job.CopyRetention)
// keeps: copy target ID, then backup ID, then the rules that keep the backup's copy
// on that target.
type CopyKeeps map[string]map[string][]KeepReason

// retain marks, on r (a record about to be soft-deleted by retention), every done
// copy the overrides keep as Retained: the purge then leaves it on its target. A nil
// CopyKeeps retains nothing.
func (k CopyKeeps) retain(r *models.BackupRecord) {
	for targetID, byBackup := range k {
		reasons, ok := byBackup[r.ID]
		if !ok {
			continue
		}
		if c := r.Copy(targetID); c != nil && c.Status == models.CopyDone {
			c.Retained, c.RetainedReason = true, JoinReasons(reasons)
		}
	}
}

// copyCandidates returns the backups of job a copy target's override decides about:
// the job's own scheduled, database-level backups from its connection that have a
// done copy on targetID and are either still completed (the copy follows its primary
// until retention deletes it) or deleted by retention with the copy retained. Each
// is a view that reads its archive from the copy (see models.BackupRecord.AtCopy)
// and counts as completed, so the same planner decides on the copies' own
// verification.
func copyCandidates(job *models.Job, targetID string, records []*models.BackupRecord) []*models.BackupRecord {
	var out []*models.BackupRecord
	for _, r := range records {
		if r.JobID != job.ID || r.ConnectionID != job.ConnectionID || r.EffectiveTrigger() != models.TriggerScheduled || r.InstanceScope() {
			continue
		}
		c := r.Copy(targetID)
		if c == nil || c.Status != models.CopyDone {
			continue
		}
		switch {
		case r.Status == models.StatusCompleted:
		case r.Status == models.StatusDeleted && c.Retained:
		default:
			continue
		}
		v := r.AtCopy(c)
		v.Status = models.StatusCompleted
		out = append(out, v)
	}
	return out
}

// PlanCopyRetention decides, at now, which backups of job the copy retention
// overrides of its copy targets keep. For every override the candidates (see
// copyCandidates) go through PlanRetentionPolicy with the override's policy, in the
// job's zone: a backup that plan does not delete is kept on that target, together
// with the rules that keep it. Nothing is changed.
func PlanCopyRetention(now time.Time, job *models.Job, records []*models.BackupRecord) CopyKeeps {
	keeps := CopyKeeps{}
	loc := job.RetentionLocation()
	for targetID, policy := range job.CopyRetention {
		plan := PlanRetentionPolicy(now, policy, loc, copyCandidates(job, targetID, records))
		byBackup := make(map[string][]KeepReason, len(plan.Kept))
		for _, k := range plan.Kept {
			byBackup[k.BackupID] = k.Reasons
		}
		keeps[targetID] = byBackup
	}
	return keeps
}

// releaseCopies clears Retained on the copies of job's deleted backups that the
// overrides in keeps no longer keep (the policy moved on, or the override was
// removed): the next purge deletes them, honouring their S3 Object Lock. It returns
// the IDs of the backups it released copies of.
func releaseCopies(ctx context.Context, metadataStore store.Store, job *models.Job, keeps CopyKeeps, records []*models.BackupRecord, logger *slog.Logger) []string {
	updater, ok := metadataStore.(BackupUpdater)
	if !ok {
		return nil
	}
	var released []string
	for _, r := range records {
		if r.JobID != job.ID || r.Status != models.StatusDeleted || !r.HasRetainedCopies() {
			continue
		}
		var drop []string
		for i := range r.Copies {
			c := &r.Copies[i]
			if _, kept := keeps[c.TargetID][r.ID]; c.Retained && !kept {
				drop = append(drop, c.TargetID)
			}
		}
		if len(drop) == 0 {
			continue
		}
		_, err := updater.UpdateBackupRecord(ctx, r.ID, func(cur *models.BackupRecord) error {
			for _, id := range drop {
				if c := cur.Copy(id); c != nil && slices.Contains(drop, c.TargetID) {
					c.Retained, c.RetainedReason = false, ""
				}
			}
			return nil
		})
		switch {
		case err == nil:
			released = append(released, r.ID)
			logger.Info("retention releases the copy of a deleted backup its policy no longer keeps", logsafe.Attr("backup_id", r.ID))
		case !errors.Is(err, store.ErrNotFound):
			logger.Warn("cannot release the retained copy of a deleted backup", logsafe.Attr("backup_id", r.ID), logsafe.Error(err))
		}
	}
	return released
}
