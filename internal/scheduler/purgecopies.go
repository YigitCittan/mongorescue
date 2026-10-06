package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// purgeCopies deletes every copy of rec whose object may exist (see
// models.BackupCopy.MayExist) from its own storage target, honouring that target's
// Object Lock: every version of a copy on a locked target is deleted, and only once
// none is locked. Each deleted copy is marked purged on the record at once, so a
// purge interrupted later does not delete it again. It returns the end of the
// longest lock still in force (the record must wait for it), or an error naming the
// copy that could not be deleted.
func (p purgeRun) purgeCopies(ctx context.Context, rec *models.BackupRecord) (*time.Time, error) {
	var lockedUntil *time.Time
	for i := range rec.Copies {
		c := rec.Copies[i]
		if !c.MayExist() {
			continue
		}
		until, err := p.purgeCopy(ctx, c)
		switch {
		case err != nil:
			p.logger.Warn("purge could not delete a backup copy; the backup is retried by the next purge",
				logsafe.Attr("backup_id", rec.ID), logsafe.Attr("storage_target_id", c.TargetID), logsafe.Error(err))
			return nil, fmt.Errorf("delete the copy of %s on %s: %w", rec.ID, c.TargetID, err)
		case until != nil:
			if lockedUntil == nil || until.After(*lockedUntil) {
				lockedUntil = until
			}
			p.recordCopy(ctx, rec.ID, c.TargetID, func(cp *models.BackupCopy) {
				at := until.UTC()
				cp.RetainUntil = &at
			})
		default:
			at := p.now.UTC()
			p.recordCopy(ctx, rec.ID, c.TargetID, func(cp *models.BackupCopy) {
				cp.Status, cp.PurgedAt = models.CopyPurged, &at
			})
		}
	}
	return lockedUntil, nil
}

// purgeCopy deletes copy c under the lock of its object. A copy already gone counts
// as deleted.
func (p purgeRun) purgeCopy(ctx context.Context, c models.BackupCopy) (*time.Time, error) {
	unlock, err := runs.LockDeletion(ctx, runs.ArchiveKey(c.TargetID, c.StorageKey))
	if err != nil {
		return nil, err
	}
	defer unlock()
	driver, err := p.storages(ctx, c.TargetID)
	if err != nil {
		return nil, err
	}
	until, err := storage.Purge(ctx, driver, c.StorageKey, c.VersionID, p.now)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return until, err
}

// recordCopy applies fn to copy targetID of the stored record id.
func (p purgeRun) recordCopy(ctx context.Context, id, targetID string, fn func(*models.BackupCopy)) {
	u, ok := p.store.(BackupUpdater)
	if !ok {
		return
	}
	if _, err := u.UpdateBackupRecord(ctx, id, func(r *models.BackupRecord) error {
		if cp := r.Copy(targetID); cp != nil {
			fn(cp)
		}
		return nil
	}); err != nil {
		p.logger.Warn("cannot record the purge of a backup copy", logsafe.Attr("backup_id", id), logsafe.Error(err))
	}
}
