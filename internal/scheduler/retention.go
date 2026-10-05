// Package scheduler manages automated cron schedules and retention pruning policies.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// MinCountPruneAge is the youngest a backup can be for count-based retention
// (RetentionCount) to delete it. A burst of runs therefore cannot push good backups
// out of the retention window: they are only pruned by count once they are a day old.
const MinCountPruneAge = 24 * time.Hour

// StorageFunc returns the storage driver of a storage target (the default target for
// an empty ID).
type StorageFunc func(ctx context.Context, targetID string) (storage.Storage, error)

// Reasons a backup selected by a retention rule is kept anyway.
const (
	// ProtectedPinned is a pinned backup (legal hold).
	ProtectedPinned = "pinned"
	// ProtectedLastVerified is the job's newest backup whose archive passed
	// verification: it is never deleted, whatever the policy.
	ProtectedLastVerified = "last_verified"
)

// RetentionDecision is a backup the retention policy deletes, and why.
type RetentionDecision struct {
	// Backup is the backup record.
	Backup *models.BackupRecord `json:"backup"`
	// Reason is the rule that selects it.
	Reason models.RetentionReason `json:"reason"`
	// Detail explains the rule ("older than 30 days").
	Detail string `json:"detail"`
}

// ProtectedBackup is a backup a retention rule selects but that is kept.
type ProtectedBackup struct {
	// BackupID identifies the backup.
	BackupID string `json:"backup_id"`
	// Reason is ProtectedPinned or ProtectedLastVerified.
	Reason string `json:"reason"`
}

// RetentionPlan is what a retention policy does to a set of backups.
type RetentionPlan struct {
	// Delete lists the backups to delete, oldest first.
	Delete []RetentionDecision `json:"delete"`
	// Protected lists the backups a rule selects but that are kept.
	Protected []ProtectedBackup `json:"protected"`
	// Considered counts the completed scheduled backups the policy applies to.
	Considered int `json:"considered"`
	// Databases breaks the plan down per database, sorted by name: a job's policy
	// applies to each of its databases separately.
	Databases []DatabaseRetention `json:"databases"`
}

// DatabaseRetention is the part of a retention plan that concerns one database.
type DatabaseRetention struct {
	// Database is the database name.
	Database string `json:"database"`
	// Considered counts its completed scheduled backups.
	Considered int `json:"considered"`
	// Delete counts the ones the policy deletes.
	Delete int `json:"delete"`
	// Protected counts the ones a rule selects but that are kept.
	Protected int `json:"protected"`
	// Kept counts the ones that stay.
	Kept int `json:"kept"`
	// LastGood is the newest completed backup, which is always kept.
	LastGood string `json:"last_good,omitempty"`
}

// PlanRetention decides which of records a policy of retentionDays and
// retentionCount deletes at now, without changing anything. PruneBackupsOn deletes
// exactly the backups it lists, so it doubles as the dry-run preview.
//
// Only completed scheduled backups (models.TriggerScheduled, see
// BackupRecord.EffectiveTrigger) are considered; on-demand, manual and MCP backups
// are never pruned automatically, and neither are PITR base backups
// (models.ScopeInstance), which their stream's retention owns. The policy applies to every database separately
// (a multi-database job keeps N backups of each of its databases), and so do the
// floors that protect the scheduled ones: the max(retentionCount, 1) most recent of
// each database are never deleted (by either rule), so a database whose backup
// failed today keeps its last good one; count-based retention never deletes a
// backup younger than MinCountPruneAge, pinned backups are never deleted, and
// neither is the newest backup of each database whose archive passed verification.
// Callers pass the records of one job (see JobRetentionHistory).
func PlanRetention(now time.Time, retentionDays, retentionCount int, records []*models.BackupRecord) RetentionPlan {
	plan := RetentionPlan{Delete: []RetentionDecision{}, Protected: []ProtectedBackup{}, Databases: []DatabaseRetention{}}
	if retentionDays <= 0 && retentionCount <= 0 {
		return plan
	}
	byDatabase := map[string][]*models.BackupRecord{}
	for _, r := range records {
		if r.Status == models.StatusCompleted && r.EffectiveTrigger() == models.TriggerScheduled && !r.InstanceScope() {
			byDatabase[r.Database] = append(byDatabase[r.Database], r)
		}
	}
	names := make([]string, 0, len(byDatabase))
	for name := range byDatabase {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		del, protected := planDatabase(now, retentionDays, retentionCount, byDatabase[name])
		successful := byDatabase[name]
		plan.Considered += len(successful)
		plan.Delete = append(plan.Delete, del...)
		plan.Protected = append(plan.Protected, protected...)
		plan.Databases = append(plan.Databases, DatabaseRetention{
			Database: name, Considered: len(successful), Delete: len(del), Protected: len(protected),
			Kept: len(successful) - len(del), LastGood: successful[0].ID,
		})
	}
	// Oldest first across databases.
	sort.SliceStable(plan.Delete, func(i, j int) bool {
		return plan.Delete[i].Backup.StartedAt.Before(plan.Delete[j].Backup.StartedAt)
	})
	return plan
}

// planDatabase plans the retention of the completed scheduled backups of one
// database; it sorts successful newest first.
func planDatabase(now time.Time, retentionDays, retentionCount int, successful []*models.BackupRecord) ([]RetentionDecision, []ProtectedBackup) {
	var del []RetentionDecision
	var protected []ProtectedBackup
	// Newest first.
	sort.SliceStable(successful, func(i, j int) bool {
		return successful[i].StartedAt.After(successful[j].StartedAt)
	})
	lastVerified := ""
	for _, r := range successful {
		if r.Verification == models.VerificationOK {
			lastVerified = r.ID
			break
		}
	}

	floor := max(retentionCount, 1)
	cutoff := now.AddDate(0, 0, -retentionDays)
	for _, rec := range successful[min(floor, len(successful)):] {
		var d *RetentionDecision
		switch {
		case retentionDays > 0 && rec.StartedAt.Before(cutoff):
			d = &RetentionDecision{Backup: rec, Reason: models.RetentionMaxAge, Detail: fmt.Sprintf("older than %d days", retentionDays)}
		case retentionCount > 0 && now.Sub(rec.StartedAt) >= MinCountPruneAge:
			d = &RetentionDecision{Backup: rec, Reason: models.RetentionMaxCount, Detail: fmt.Sprintf("beyond the %d newest backups", retentionCount)}
		}
		switch {
		case d == nil:
		case rec.Pinned:
			protected = append(protected, ProtectedBackup{BackupID: rec.ID, Reason: ProtectedPinned})
		case rec.ID == lastVerified:
			protected = append(protected, ProtectedBackup{BackupID: rec.ID, Reason: ProtectedLastVerified})
		default:
			del = append(del, *d)
		}
	}
	slices.Reverse(del)
	return del, protected
}

// JobRetentionHistory returns the records job's retention applies to: the job's own
// scheduled backups taken from its connection and stored on targetID. On-demand,
// manual and MCP backups neither count towards the kept ones nor get pruned. The
// same database name on another server is a different dataset, and retention counts
// per storage target: backups kept on another target (e.g. before the job was
// moved) are not pruned by this one.
func JobRetentionHistory(job *models.Job, targetID string, records []*models.BackupRecord) []*models.BackupRecord {
	out := make([]*models.BackupRecord, 0, len(records))
	for _, r := range records {
		if r.JobID == job.ID && r.EffectiveTrigger() == models.TriggerScheduled && !r.InstanceScope() &&
			r.ConnectionID == job.ConnectionID && r.StorageTargetID == targetID {
			out = append(out, r)
		}
	}
	return out
}

// RetentionActor is the DeletedBy of backups deleted by retention.
const RetentionActor = "retention"

// PruneBackups applies a retention policy of retentionDays and retentionCount to
// records (the records of one job) now: the backups PlanRetention lists are deleted
// softly (models.StatusDeleted) with the delete grace period grace, and their
// archives stay until the purge (PurgeDeleted) removes them once grace has passed.
// It returns the IDs of the deleted backups.
func PruneBackups(
	ctx context.Context,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	grace time.Duration,
	logger *slog.Logger,
) ([]string, error) {
	return prune(ctx, time.Now().UTC(), grace, retentionDays, retentionCount, records, metadataStore, logger, nil)
}

// BackupUpdater updates a stored backup record atomically (implemented by
// *store.SQLiteStore). Retention uses it to re-check a backup right before it is
// pruned, so a pin set meanwhile is never overridden.
type BackupUpdater interface {
	// UpdateBackupRecord applies fn to the stored record id in one transaction.
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
}

// ArchiveRefs lists every backup row, readable or not, that names an archive
// (implemented by *store.SQLiteStore).
type ArchiveRefs interface {
	// ArchiveReferenceIDs returns the IDs of the rows naming key on targetID.
	ArchiveReferenceIDs(ctx context.Context, targetID, key string) ([]string, error)
}

// BackupPruner deletes a backup record for retention in one transaction that
// re-checks pins and the job's last verified backup (implemented by
// *store.SQLiteStore).
type BackupPruner interface {
	// PruneBackupRecord applies mark to backup id or refuses with
	// store.ErrPruneRefused.
	PruneBackupRecord(ctx context.Context, id string, mark func(*models.BackupRecord)) (*models.BackupRecord, error)
}

// errRetentionSkip aborts the prune of a backup that changed since it was planned.
var errRetentionSkip = errors.New("scheduler: backup changed since retention was planned")

// prune implements PruneBackups at now with the grace period grace; onDeleted
// (optional) is called for every deleted backup.
func prune(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	logger *slog.Logger,
	onDeleted func(ctx context.Context, e models.RetentionLogEntry),
) ([]string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	plan := PlanRetention(now, retentionDays, retentionCount, records)
	for _, p := range plan.Protected {
		logger.Info("retention keeps a backup its policy selects",
			logsafe.Attr("backup_id", p.BackupID), slog.String("reason", p.Reason))
	}
	if len(plan.Delete) == 0 {
		return nil, nil
	}

	var prunedIDs []string
	updater, atomic := metadataStore.(BackupUpdater)
	pruner, transactional := metadataStore.(BackupPruner)
	purgeAfter := now.Add(grace)
	for _, d := range plan.Delete {
		// Deletions of one job's backups (retention, single and bulk deletes, the
		// purge) hold the job's deletion lock, so their protections are decided on the
		// current state.
		err := func() error {
			rec := d.Backup
			unlock, err := runs.LockDeletion(ctx, runs.DeletionKey(rec.JobID, rec.ConnectionID, rec.Database))
			if err != nil {
				return err
			}
			defer unlock()
			logger.Info("deleting expired backup per retention policy; its archive is kept until the grace period ends",
				logsafe.Attr("backup_id", rec.ID),
				logsafe.Attr("database", rec.Database),
				slog.Time("started_at", rec.StartedAt),
				slog.String("reason", string(d.Reason)),
				slog.Time("purge_after", purgeAfter),
			)
			mark := func(r *models.BackupRecord) {
				r.MarkDeleted(models.SoftDelete{At: now, PurgeAfter: purgeAfter, By: RetentionActor, Reason: d.Detail})
			}

			// Mark the record deleted, re-checking it: a backup pinned (or otherwise
			// changed) since it was listed is skipped. The archive is not touched.
			var markErr error
			if transactional {
				// Pins and the last verified backup are re-checked in the same transaction.
				_, markErr = pruner.PruneBackupRecord(ctx, rec.ID, mark)
				if errors.Is(markErr, store.ErrPruneRefused) {
					logger.Info("retention keeps a backup it planned to delete", logsafe.Attr("backup_id", rec.ID), logsafe.Error(markErr))
					return nil
				}
			} else if atomic {
				_, markErr = updater.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error {
					if r.Pinned || r.Status != models.StatusCompleted {
						return errRetentionSkip
					}
					mark(r)
					return nil
				})
			} else {
				mark(rec)
				markErr = metadataStore.SaveBackupRecord(ctx, rec)
			}
			switch {
			case errors.Is(markErr, errRetentionSkip), errors.Is(markErr, store.ErrNotFound):
				logger.Info("retention skips a backup that changed since it was planned", logsafe.Attr("backup_id", rec.ID))
				return nil
			case markErr != nil:
				logger.Error("failed to mark a backup deleted by retention",
					logsafe.Attr("backup_id", rec.ID),
					logsafe.Error(markErr),
				)
				return fmt.Errorf("delete backup %s for retention: %w", rec.ID, markErr)
			}
			rec.Status = models.StatusDeleted

			after := purgeAfter
			if onDeleted != nil {
				onDeleted(ctx, models.RetentionLogEntry{
					Time: now, JobID: rec.JobID, BackupID: rec.ID, Database: rec.Database,
					StorageTargetID: rec.StorageTargetID, StorageKey: rec.StorageKey, BackupStartedAt: rec.StartedAt,
					SizeBytes: rec.SizeBytes, Reason: d.Reason, Detail: d.Detail, PurgeAfter: &after,
				})
			}
			prunedIDs = append(prunedIDs, rec.ID)
			return nil
		}()
		if err != nil {
			return prunedIDs, err
		}
	}

	logger.Info("retention completed: backups deleted, archives kept until their grace period ends",
		slog.Int("deleted_count", len(prunedIDs)),
	)

	return prunedIDs, nil
}
