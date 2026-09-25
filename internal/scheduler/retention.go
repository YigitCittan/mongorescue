// Package scheduler manages automated cron schedules and retention pruning policies.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
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
// or job. It deletes expired archives from the storage target each record names
// (resolved through storages) and marks the records as pruned in the metadata store.
//
// Only completed scheduled backups (models.TriggerScheduled, see
// BackupRecord.EffectiveTrigger) are considered; on-demand, manual and MCP backups are
// never pruned automatically. Two floors protect the scheduled ones: the
// max(retentionCount, 1) most recent are never pruned (by either rule), and
// count-based retention never prunes a backup younger than MinCountPruneAge. Callers
// pass the records of one job.
func PruneBackupsOn(
	ctx context.Context,
	retentionDays int,
	retentionCount int,
	records []*models.BackupRecord,
	metadataStore store.Store,
	storages StorageFunc,
	logger *slog.Logger,
) ([]string, error) {
	if retentionDays <= 0 && retentionCount <= 0 {
		// No retention policy configured (keep indefinitely)
		return nil, nil
	}

	if logger == nil {
		logger = slog.Default()
	}

	// Filter successful scheduled backups only
	var successful []*models.BackupRecord
	for _, r := range records {
		if r.Status == models.StatusCompleted && r.EffectiveTrigger() == models.TriggerScheduled {
			successful = append(successful, r)
		}
	}

	if len(successful) == 0 {
		return nil, nil
	}

	// Sort newest first
	sort.Slice(successful, func(i, j int) bool {
		return successful[i].StartedAt.After(successful[j].StartedAt)
	})

	toPruneMap := make(map[string]*models.BackupRecord)
	now := time.Now().UTC()
	// The newest `floor` completed backups are always kept.
	floor := max(retentionCount, 1)

	// 1. Time-based retention (RetentionDays)
	if retentionDays > 0 {
		cutoff := now.AddDate(0, 0, -retentionDays)
		for _, rec := range successful[min(floor, len(successful)):] {
			if rec.StartedAt.Before(cutoff) {
				toPruneMap[rec.ID] = rec
			}
		}
	}

	// 2. Count-based retention (RetentionCount), for backups old enough only
	if retentionCount > 0 {
		for _, rec := range successful[min(floor, len(successful)):] {
			if now.Sub(rec.StartedAt) >= MinCountPruneAge {
				toPruneMap[rec.ID] = rec
			}
		}
	}

	if len(toPruneMap) == 0 {
		return nil, nil
	}

	var prunedIDs []string

	// Delete from storage and update status
	for id, rec := range toPruneMap {
		logger.Info("pruning expired backup per retention policy",
			slog.String("backup_id", id),
			slog.String("database", rec.Database),
			slog.String("storage_key", rec.StorageKey),
			slog.Time("started_at", rec.StartedAt),
		)

		// Delete the physical archive from the record's own storage target
		if rec.StorageKey != "" {
			storageDriver, err := storages(ctx, rec.StorageTargetID)
			if err == nil {
				err = storageDriver.Delete(ctx, rec.StorageKey)
			}
			if err != nil {
				logger.Warn("failed to delete physical backup file from storage during pruning",
					slog.String("backup_id", id),
					slog.String("storage_key", rec.StorageKey),
					slog.Any("error", err),
				)
			}
		}

		// Update record status to pruned
		rec.Status = models.StatusPruned
		if err := metadataStore.SaveBackupRecord(ctx, rec); err != nil {
			logger.Error("failed to update backup record status to pruned",
				slog.String("backup_id", id),
				slog.Any("error", err),
			)
			return prunedIDs, fmt.Errorf("update pruned record %s: %w", id, err)
		}

		prunedIDs = append(prunedIDs, id)
	}

	logger.Info("retention pruning completed",
		slog.Int("pruned_count", len(prunedIDs)),
	)

	return prunedIDs, nil
}
