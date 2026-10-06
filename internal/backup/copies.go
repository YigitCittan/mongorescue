package backup

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

// ErrCopyFailed means a synchronous copy (models.CopySync) of a backup failed: the
// backup is failed, and its archive and the copies already made are deleted.
var ErrCopyFailed = errors.New("backup: copying the archive to a copy target failed")

// CopyFunc copies the archive of a completed record to every pending copy of it
// (record.Copies) and records the outcome on them; mbps caps the upload (0 =
// unlimited). It is implemented by copies.Service.CopyAll.
type CopyFunc func(ctx context.Context, record *models.BackupRecord, mbps float64) error

// WithCopier makes backups with synchronous copies (models.CopySync) copy their
// archive with fn before they complete. Without it such a backup fails.
func WithCopier(fn CopyFunc) Option {
	return func(e *Engine) { e.copier = fn }
}

// copySync aligns the storage key of record's pending copies with its archive (the
// encryption may have changed it since Prepare) and, for a synchronous backup,
// copies the archive now. A failed copy deletes the archive and the copies made.
func (e *Engine) copySync(ctx context.Context, opts models.BackupOptions, record *models.BackupRecord) error {
	for i := range record.Copies {
		if record.Copies[i].Status == models.CopyPending {
			record.Copies[i].StorageKey = record.StorageKey
		}
	}
	if len(record.Copies) == 0 || !record.CopyMode.Sync() {
		return nil
	}
	tracker := runs.FromContext(ctx)
	if e.copier == nil {
		e.deleteArtifact(ctx, record, record.StorageKey)
		return fmt.Errorf("%w: copies are not available", ErrCopyFailed)
	}
	tracker.Printf("copying the archive to %d copy target(s)", len(record.Copies))
	if err := e.copier(ctx, record, e.uploadMbps(opts)); err != nil {
		e.logger.Error("synchronous backup copy failed; deleting the archive and its copies",
			logsafe.Attr("backup_id", record.ID), logsafe.Error(err))
		e.deleteArtifact(ctx, record, record.StorageKey)
		e.deleteCopies(ctx, record)
		return fmt.Errorf("%w: %w", ErrCopyFailed, err)
	}
	tracker.Printf("archive copied to %d copy target(s)", len(record.Copies))
	return nil
}

// deleteCopies removes the copies a failed synchronous backup made. A copy under
// its S3 Object Lock is kept with its retention and the record is marked
// ArchiveCleanupPending: the scheduler's purge deletes it once the lock has ended.
func (e *Engine) deleteCopies(ctx context.Context, record *models.BackupRecord) {
	if e.storageFor == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	for i := range record.Copies {
		cp := &record.Copies[i]
		// A copy that was made, or a failed one that left an object behind.
		if cp.Status == models.CopyPending || !cp.MayExist() {
			continue
		}
		driver, err := e.storageFor(cleanupCtx, cp.TargetID)
		var until *time.Time
		if err == nil {
			until, err = storage.Purge(cleanupCtx, driver, cp.StorageKey, cp.VersionID, time.Now())
		}
		switch {
		case err != nil && !errors.Is(err, storage.ErrNotFound):
			e.logger.Warn("failed to delete the copy of a failed backup", logsafe.Attr("backup_id", record.ID),
				logsafe.Attr("storage_target_id", cp.TargetID), logsafe.Error(err))
			record.ArchiveCleanupPending = true
		case until != nil:
			at := until.UTC()
			cp.RetainUntil, record.ArchiveCleanupPending = &at, true
		default:
			now := time.Now().UTC()
			cp.Status, cp.PurgedAt = models.CopyPurged, &now
		}
	}
}
