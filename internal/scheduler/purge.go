package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// maintenanceSchedule is how often the scheduler purges deleted backups whose grace
// period has ended and applies the maintenance hooks (pending protection changes).
const maintenanceSchedule = "@every 10m"

// AuditToolPurge is the audit log "tool" of purges.
const AuditToolPurge = "backup.purge"

// PurgeOutcome is what the purge did to one deleted backup.
type PurgeOutcome struct {
	// Backup is the purged record (StatusPurged).
	Backup *models.BackupRecord
	// ArchiveDeleted reports that the archive was removed from storage (false when
	// it was already gone or another record still names it).
	ArchiveDeleted bool
	// ArchiveKept names the record that still holds the archive, if any.
	ArchiveKept string
}

// errPurgeSkip aborts the purge of a backup that changed since it was listed.
var errPurgeSkip = errors.New("scheduler: backup is no longer due for its purge")

// holdsArchive reports whether a record in state st owns its archive: its object is
// expected to exist, now or once undeleted (see integrity's live states).
func holdsArchive(st models.BackupStatus) bool {
	switch st {
	case models.StatusCompleted, models.StatusMissing, models.StatusInProgress, models.StatusPending, models.StatusDeleted:
		return true
	default:
		return false
	}
}

// PurgeDeleted purges, at now, every deleted backup whose grace period has passed
// (models.BackupRecord.PurgeDue with grace, the grace period in force): it removes
// the archive from the record's own storage target, unless another record still
// holds it, and marks the record purged. Each purge holds the deletion lock of the
// backup's job and its archive's lock and re-reads the record inside them, so an
// undelete, a pin or a grace period raised meanwhile is honoured. A backup whose
// archive cannot be deleted stays deleted and is retried by the next run. onPurged
// (optional) is called for every purged backup. It returns the purged IDs.
func PurgeDeleted(
	ctx context.Context,
	now time.Time,
	grace time.Duration,
	metadataStore store.Store,
	storages StorageFunc,
	logger *slog.Logger,
	onPurged func(ctx context.Context, o PurgeOutcome),
) ([]string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	page, err := metadataStore.QueryBackupRecords(ctx, store.BackupFilter{Status: models.StatusDeleted, Sort: store.SortOldest})
	if err != nil {
		return nil, fmt.Errorf("list deleted backups: %w", err)
	}
	var purged []string
	var errs []error
	for _, row := range page.Rows {
		if !row.Record.PurgeDue(now, grace) {
			continue
		}
		if ctx.Err() != nil {
			return purged, ctx.Err()
		}
		o, err := purgeOne(ctx, now, grace, row.Record, metadataStore, storages, logger)
		switch {
		case errors.Is(err, errPurgeSkip):
		case err != nil:
			errs = append(errs, err)
		default:
			purged = append(purged, o.Backup.ID)
			if onPurged != nil {
				onPurged(ctx, o)
			}
		}
	}
	if len(purged) > 0 || len(errs) > 0 {
		logger.Info("purge of deleted backups finished", slog.Int("purged", len(purged)), slog.Int("failed", len(errs)))
	}
	return purged, errors.Join(errs...)
}

// purgeOne purges the deleted backup listed as rec (see PurgeDeleted).
func purgeOne(ctx context.Context, now time.Time, grace time.Duration, rec *models.BackupRecord,
	metadataStore store.Store, storages StorageFunc, logger *slog.Logger) (PurgeOutcome, error) {
	unlock, err := runs.LockDeletion(ctx, runs.DeletionKey(rec.JobID, rec.ConnectionID, rec.Database))
	if err != nil {
		return PurgeOutcome{}, err
	}
	defer unlock()
	current, err := metadataStore.GetBackupRecord(ctx, rec.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return PurgeOutcome{}, errPurgeSkip
	case err != nil:
		return PurgeOutcome{}, fmt.Errorf("load deleted backup %s: %w", rec.ID, err)
	case !current.PurgeDue(now, grace):
		return PurgeOutcome{}, errPurgeSkip
	}
	out := PurgeOutcome{Backup: current}
	if current.StorageKey != "" {
		unlockArchive, lockErr := runs.LockDeletion(ctx, runs.ArchiveKey(current.StorageTargetID, current.StorageKey))
		if lockErr != nil {
			return PurgeOutcome{}, lockErr
		}
		defer unlockArchive()
		holder, refErr := archiveHolder(ctx, metadataStore, current)
		if refErr != nil {
			logger.Warn("purge cannot check for shared archives; keeping the backup deleted", logsafe.Attr("backup_id", current.ID), logsafe.Error(refErr))
			return PurgeOutcome{}, fmt.Errorf("check the archive references of %s: %w", current.ID, refErr)
		}
		if holder != "" {
			out.ArchiveKept = holder
		} else {
			driver, delErr := storages(ctx, current.StorageTargetID)
			if delErr == nil {
				delErr = driver.Delete(ctx, current.StorageKey)
			}
			switch {
			case delErr == nil:
				out.ArchiveDeleted = true
			case errors.Is(delErr, storage.ErrNotFound):
				// Already gone (removed by hand or by a lifecycle rule).
			default:
				logger.Warn("purge could not delete an archive; the backup stays deleted and is retried",
					logsafe.Attr("backup_id", current.ID), logsafe.Attr("storage_target_id", current.StorageTargetID),
					logsafe.Attr("storage_key", current.StorageKey), logsafe.Error(delErr))
				return PurgeOutcome{}, fmt.Errorf("delete the archive of %s: %w", current.ID, delErr)
			}
		}
	}
	at := now.UTC()
	markPurged := func(r *models.BackupRecord) error {
		if !r.PurgeDue(now, grace) {
			return errPurgeSkip
		}
		r.Status, r.PurgedAt = models.StatusPurged, &at
		return nil
	}
	if u, ok := metadataStore.(BackupUpdater); ok {
		current, err = u.UpdateBackupRecord(ctx, current.ID, markPurged)
	} else if err = markPurged(current); err == nil {
		err = metadataStore.SaveBackupRecord(ctx, current)
	}
	switch {
	case errors.Is(err, errPurgeSkip), errors.Is(err, store.ErrNotFound):
		return PurgeOutcome{}, errPurgeSkip
	case err != nil:
		return PurgeOutcome{}, fmt.Errorf("mark %s purged: %w", rec.ID, err)
	}
	out.Backup = current
	logger.Info("deleted backup purged after its grace period",
		logsafe.Attr("backup_id", current.ID), logsafe.Attr("database", current.Database),
		slog.Bool("archive_deleted", out.ArchiveDeleted))
	return out, nil
}

// archiveHolder returns the ID of another record that still holds rec's archive (see
// holdsArchive), or "". A row that cannot be read holds it.
func archiveHolder(ctx context.Context, metadataStore store.Store, rec *models.BackupRecord) (string, error) {
	var ids []string
	if refs, ok := metadataStore.(ArchiveRefs); ok {
		found, err := refs.ArchiveReferenceIDs(ctx, rec.StorageTargetID, rec.StorageKey)
		if err != nil {
			return "", err
		}
		ids = found
	} else {
		all, err := metadataStore.ListBackupRecords(ctx, "")
		if err != nil {
			return "", err
		}
		for _, o := range models.ArchiveReferences(all, rec) {
			ids = append(ids, o.ID)
		}
	}
	for _, id := range ids {
		if id == rec.ID {
			continue
		}
		o, err := metadataStore.GetBackupRecord(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return id, nil
		case holdsArchive(o.Status):
			return id, nil
		}
	}
	return "", nil
}

// runMaintenance is the cron callback of maintenanceSchedule: it purges the deleted
// backups whose grace period has ended, then runs the maintenance hooks. It is
// tracked like a scheduled run, so Stop waits for it.
func (s *Scheduler) runMaintenance() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	ctx := s.ctx
	s.inflight.Add(1)
	s.mu.Unlock()
	defer s.inflight.Done()
	s.Maintain(ctx)
}

// Maintain purges the deleted backups whose grace period has ended (PurgeDeleted)
// and runs the maintenance hooks (WithMaintenance). The scheduler calls it every ten
// minutes; tests call it directly.
func (s *Scheduler) Maintain(ctx context.Context) {
	if _, err := PurgeDeleted(ctx, s.clock(), s.deleteGrace(), s.metadataStore, s.storageFor, s.logger, s.purged); err != nil {
		s.logger.Warn("purge of deleted backups incomplete", logsafe.Error(err))
	}
	for _, fn := range s.maintenance {
		fn(ctx)
	}
}

// purged records one purge: the run log is removed, and the audit log and a
// security.destructive_action event record it.
func (s *Scheduler) purged(ctx context.Context, o PurgeOutcome) {
	rec := o.Backup
	if s.registry != nil {
		if err := s.registry.RemoveLog(rec.ID); err != nil {
			s.logger.Warn("failed to delete the log of a purged backup", logsafe.Attr("backup_id", rec.ID), logsafe.Error(err))
		}
	}
	detail := "backup " + rec.ID + " (db " + rec.Database + ") purged after its grace period"
	if o.ArchiveKept != "" {
		detail += "; its archive is kept: it also belongs to backup " + o.ArchiveKept
	}
	if s.auditor != nil {
		args, _ := json.Marshal(map[string]any{
			"backup_id": rec.ID, "job_id": rec.JobID, "database": rec.Database, "storage_target_id": rec.StorageTargetID,
			"deleted_by": rec.DeletedBy, "archive_deleted": o.ArchiveDeleted, "archive_kept_by": o.ArchiveKept,
		})
		s.auditor.Record(ctx, audit.Entry{
			Time: s.clock().UTC(), APIKeyName: "purge", Transport: audit.TransportSystem,
			Tool: AuditToolPurge, Arguments: args, Result: audit.ResultOK,
		})
	}
	if s.publisher != nil {
		e := events.SecurityEvent(events.SecurityDestructiveAction, s.clock(), "purge", "system", "", detail)
		e.BackupID, e.JobID, e.Database, e.TargetID = rec.ID, rec.JobID, rec.Database, rec.StorageTargetID
		s.publisher.Publish(ctx, e)
	}
}

// clock returns the scheduler's current time (WithClock).
func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// deleteGrace returns the delete grace period in force (WithDeleteGrace).
func (s *Scheduler) deleteGrace() time.Duration {
	if s.grace != nil {
		return s.grace()
	}
	return models.GraceDuration(models.DefaultDeleteGraceDays)
}

// WithClock sets the scheduler's time source for retention and the purge (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) { s.now = now }
}

// WithDeleteGrace sets the delete grace period in force (security.delete_grace_days),
// read for every retention run and purge. Without it the default of
// models.DefaultDeleteGraceDays applies.
func WithDeleteGrace(grace func() time.Duration) Option {
	return func(s *Scheduler) { s.grace = grace }
}

// WithMaintenance adds fn to the hooks Maintain runs every ten minutes after the
// purge (the operations service applies due pending changes and expires approvals).
func WithMaintenance(fn func(ctx context.Context)) Option {
	return func(s *Scheduler) { s.maintenance = append(s.maintenance, fn) }
}
