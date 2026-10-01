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

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
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
}

// PlanRetention decides which of records a policy of retentionDays and
// retentionCount deletes at now, without changing anything. PruneBackupsOn deletes
// exactly the backups it lists, so it doubles as the dry-run preview.
//
// Only completed scheduled backups (models.TriggerScheduled, see
// BackupRecord.EffectiveTrigger) are considered; on-demand, manual and MCP backups
// are never pruned automatically. Floors protect the scheduled ones: the
// max(retentionCount, 1) most recent are never deleted (by either rule), count-based
// retention never deletes a backup younger than MinCountPruneAge, pinned backups are
// never deleted, and neither is the newest backup whose archive passed verification.
// Callers pass the records of one job (see JobRetentionHistory).
func PlanRetention(now time.Time, retentionDays, retentionCount int, records []*models.BackupRecord) RetentionPlan {
	plan := RetentionPlan{Delete: []RetentionDecision{}, Protected: []ProtectedBackup{}}
	if retentionDays <= 0 && retentionCount <= 0 {
		return plan
	}
	var successful []*models.BackupRecord
	for _, r := range records {
		if r.Status == models.StatusCompleted && r.EffectiveTrigger() == models.TriggerScheduled {
			successful = append(successful, r)
		}
	}
	plan.Considered = len(successful)
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
			plan.Protected = append(plan.Protected, ProtectedBackup{BackupID: rec.ID, Reason: ProtectedPinned})
		case rec.ID == lastVerified:
			plan.Protected = append(plan.Protected, ProtectedBackup{BackupID: rec.ID, Reason: ProtectedLastVerified})
		default:
			plan.Delete = append(plan.Delete, *d)
		}
	}
	slices.Reverse(plan.Delete)
	return plan
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
		if r.JobID == job.ID && r.EffectiveTrigger() == models.TriggerScheduled &&
			r.ConnectionID == job.ConnectionID && r.StorageTargetID == targetID {
			out = append(out, r)
		}
	}
	return out
}

// PruneBackups applies PruneBackupsOn with every archive stored on storageDriver.
func PruneBackups(
	ctx context.Context,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	storageDriver storage.Storage,
	logger *slog.Logger,
) ([]string, error) {
	fixed := func(context.Context, string) (storage.Storage, error) { return storageDriver, nil }
	return PruneBackupsOn(ctx, retentionDays, retentionCount, records, metadataStore, fixed, logger)
}

// PruneBackupsOn executes retention policies against existing backups for a database
// or job: it deletes the backups PlanRetention lists from the storage target each
// record names (resolved through storages) and marks the records as pruned in the
// metadata store. Callers pass the records of one job.
func PruneBackupsOn(
	ctx context.Context,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	storages StorageFunc,
	logger *slog.Logger,
) ([]string, error) {
	return prune(ctx, time.Now().UTC(), retentionDays, retentionCount, records, metadataStore, storages, logger, nil)
}

// BackupUpdater updates a stored backup record atomically (implemented by
// *store.SQLiteStore). Retention uses it to re-check a backup right before it is
// pruned, so a pin set meanwhile is never overridden.
type BackupUpdater interface {
	// UpdateBackupRecord applies fn to the stored record id in one transaction.
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
}

// BackupPruner prunes a backup record in one transaction that re-checks pins and
// the job's last verified backup (implemented by *store.SQLiteStore).
type BackupPruner interface {
	// PruneBackupRecord marks backup id pruned or refuses with store.ErrPruneRefused.
	PruneBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error)
}

// errRetentionSkip aborts the prune of a backup that changed since it was planned.
var errRetentionSkip = errors.New("scheduler: backup changed since retention was planned")

// prune implements PruneBackupsOn at now; onDeleted (optional) is called for every
// pruned backup.
func prune(
	ctx context.Context,
	now time.Time,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	storages StorageFunc,
	logger *slog.Logger,
	onDeleted func(ctx context.Context, e models.RetentionLogEntry),
) ([]string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	plan := PlanRetention(now, retentionDays, retentionCount, records)
	for _, p := range plan.Protected {
		logger.Info("retention keeps a backup its policy selects",
			slog.String("backup_id", p.BackupID), slog.String("reason", p.Reason))
	}
	if len(plan.Delete) == 0 {
		return nil, nil
	}

	var prunedIDs []string
	updater, atomic := metadataStore.(BackupUpdater)
	pruner, transactional := metadataStore.(BackupPruner)
	// Every record, to find archives another record shares (loaded once, on demand).
	var all []*models.BackupRecord
	allLoaded := false
	for _, d := range plan.Delete {
		rec := d.Backup
		logger.Info("pruning expired backup per retention policy",
			slog.String("backup_id", rec.ID),
			slog.String("database", rec.Database),
			slog.String("storage_key", rec.StorageKey),
			slog.Time("started_at", rec.StartedAt),
			slog.String("reason", string(d.Reason)),
		)

		// Mark the record pruned first, re-checking it: a backup pinned (or otherwise
		// changed) since it was listed is skipped.
		var markErr error
		if transactional {
			// Pins and the last verified backup are re-checked in the same transaction.
			_, markErr = pruner.PruneBackupRecord(ctx, rec.ID)
			if errors.Is(markErr, store.ErrPruneRefused) {
				logger.Info("retention keeps a backup it planned to prune", slog.String("backup_id", rec.ID), slog.Any("reason", markErr))
				continue
			}
		} else if atomic {
			_, markErr = updater.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error {
				if r.Pinned || r.Status != models.StatusCompleted {
					return errRetentionSkip
				}
				r.Status = models.StatusPruned
				return nil
			})
		} else {
			rec.Status = models.StatusPruned
			markErr = metadataStore.SaveBackupRecord(ctx, rec)
		}
		switch {
		case errors.Is(markErr, errRetentionSkip), errors.Is(markErr, store.ErrNotFound):
			logger.Info("retention skips a backup that changed since it was planned", slog.String("backup_id", rec.ID))
			continue
		case markErr != nil:
			logger.Error("failed to update backup record status to pruned",
				slog.String("backup_id", rec.ID),
				slog.Any("error", markErr),
			)
			return prunedIDs, fmt.Errorf("update pruned record %s: %w", rec.ID, markErr)
		}
		rec.Status = models.StatusPruned

		// Delete the physical archive from the record's own storage target
		entry := models.RetentionLogEntry{
			Time: time.Now().UTC(), JobID: rec.JobID, BackupID: rec.ID, Database: rec.Database,
			StorageTargetID: rec.StorageTargetID, StorageKey: rec.StorageKey, BackupStartedAt: rec.StartedAt,
			SizeBytes: rec.SizeBytes, Reason: d.Reason, Detail: d.Detail,
		}
		// Only an archive no other record names is deleted from storage.
		var listErr error
		if !allLoaded && rec.StorageKey != "" {
			if all, listErr = metadataStore.ListBackupRecords(ctx, ""); listErr == nil {
				allLoaded = true
			}
		}
		shared := models.ArchiveReferences(all, rec)
		switch {
		case rec.StorageKey == "":
		case listErr != nil:
			logger.Warn("retention could not check for shared archives; keeping the archive", slog.Any("error", listErr))
			entry.Detail += "; archive kept: shared archives could not be checked"
		case len(shared) > 0:
			entry.Detail += "; archive kept: it also belongs to backup " + shared[0].ID
		default:
			storageDriver, err := storages(ctx, rec.StorageTargetID)
			if err == nil {
				err = storageDriver.Delete(ctx, rec.StorageKey)
			}
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				entry.Error = redact.Text(err.Error())
				logger.Warn("failed to delete physical backup file from storage during pruning",
					slog.String("backup_id", rec.ID),
					slog.String("storage_key", rec.StorageKey),
					slog.Any("error", err),
				)
			}
		}
		if onDeleted != nil {
			onDeleted(ctx, entry)
		}
		prunedIDs = append(prunedIDs, rec.ID)
	}

	logger.Info("retention pruning completed",
		slog.Int("pruned_count", len(prunedIDs)),
	)

	return prunedIDs, nil
}
