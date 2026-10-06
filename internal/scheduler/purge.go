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
	return purgeRun{now: now, grace: func() time.Duration { return grace }, store: metadataStore, storages: storages,
		logger: logger, onPurged: onPurged}.run(ctx)
}

// LocateFunc returns where key on storage target targetID is physically stored, in
// a form that compares equal for two targets naming the same object (implemented by
// targets.Service.ObjectLocation).
type LocateFunc func(ctx context.Context, targetID, key string) (string, error)

// objectLocator locates objects across storage targets (implemented by
// *targets.Service).
type objectLocator interface {
	ObjectLocation(ctx context.Context, targetID, key string) (string, error)
}

// allArchiveRefs lists every live backup row on every target that names an archive
// (implemented by *store.SQLiteStore).
type allArchiveRefs interface {
	ArchiveRefs(ctx context.Context) ([]store.ArchiveRef, error)
}

// purgeRun is one purge (see PurgeDeleted). grace is read again for every backup,
// under its deletion lock, so a grace period raised while the purge runs is
// honoured. locate (optional) makes the shared-archive check physical: no live
// record on any target may resolve to the object about to be deleted.
type purgeRun struct {
	now      time.Time
	grace    func() time.Duration
	store    store.Store
	storages StorageFunc
	locate   LocateFunc
	logger   *slog.Logger
	onPurged func(ctx context.Context, o PurgeOutcome)
	// holders is the location index of this run (see physicalHolder), set by run.
	holders *holderIndex
}

// run purges every due deleted backup.
func (p purgeRun) run(ctx context.Context) ([]string, error) {
	p.holders = &holderIndex{}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	page, err := p.store.QueryBackupRecords(ctx, store.BackupFilter{Status: models.StatusDeleted, Sort: store.SortOldest})
	if err != nil {
		return nil, fmt.Errorf("list deleted backups: %w", err)
	}
	var purged []string
	var errs []error
	for _, row := range page.Rows {
		// A first look without the lock; purgeOne decides again under it.
		if !row.Record.PurgeDue(p.now, p.grace()) {
			continue
		}
		if ctx.Err() != nil {
			return purged, ctx.Err()
		}
		o, err := p.purgeOne(ctx, row.Record)
		switch {
		case errors.Is(err, errPurgeSkip):
		case err != nil:
			errs = append(errs, err)
		default:
			purged = append(purged, o.Backup.ID)
			if p.onPurged != nil {
				p.onPurged(ctx, o)
			}
		}
	}
	if len(purged) > 0 || len(errs) > 0 {
		p.logger.Info("purge of deleted backups finished", slog.Int("purged", len(purged)), slog.Int("failed", len(errs)))
	}
	if err := p.cleanupLockedArtifacts(ctx); err != nil {
		errs = append(errs, err)
	}
	return purged, errors.Join(errs...)
}

// pendingCleanups lists the failed backups whose locked artifact waits for its
// purge (implemented by *store.SQLiteStore).
type pendingCleanups interface {
	PendingArchiveCleanups(ctx context.Context) ([]*models.BackupRecord, error)
}

// cleanupLockedArtifacts deletes the artifacts of failed and cancelled backups that
// could not be deleted when the backup failed because of their S3 Object Lock
// (models.BackupRecord.ArchiveCleanupPending): once the lock has ended, every
// version is deleted and the flag cleared. Each runs under the archive's lock and
// is skipped while another live record names the same object.
func (p purgeRun) cleanupLockedArtifacts(ctx context.Context) error {
	lister, ok := p.store.(pendingCleanups)
	updater, canUpdate := p.store.(BackupUpdater)
	if !ok || !canUpdate {
		return nil
	}
	list, err := lister.PendingArchiveCleanups(ctx)
	if err != nil {
		return fmt.Errorf("list locked artifacts of failed backups: %w", err)
	}
	var errs []error
	for _, rec := range list {
		if rec.LockedAt(p.now) || rec.StorageKey == "" {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.cleanupOne(ctx, updater, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cleanupOne deletes the locked artifact of failed backup rec (see
// cleanupLockedArtifacts).
func (p purgeRun) cleanupOne(ctx context.Context, updater BackupUpdater, rec *models.BackupRecord) error {
	unlock, err := runs.LockDeletion(ctx, runs.ArchiveKey(rec.StorageTargetID, rec.StorageKey))
	if err != nil {
		return err
	}
	defer unlock()
	if holder, refErr := archiveHolder(ctx, p.store, rec); refErr != nil || holder != "" {
		return refErr
	}
	driver, err := p.storages(ctx, rec.StorageTargetID)
	var until *time.Time
	if err == nil {
		until, err = storage.Purge(ctx, driver, rec.StorageKey, rec.StorageVersionID, p.now)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		p.logger.Warn("cannot delete the locked artifact of a failed backup; retried by the next purge",
			logsafe.Attr("backup_id", rec.ID), logsafe.Error(err))
		return fmt.Errorf("delete the artifact of failed backup %s: %w", rec.ID, err)
	}
	_, err = updater.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error {
		if until != nil {
			at := until.UTC()
			r.RetainUntil = &at
			return nil
		}
		r.ArchiveCleanupPending = false
		return nil
	})
	if err == nil && until == nil {
		p.logger.Info("deleted the locked artifact of a failed backup after its lock ended", logsafe.Attr("backup_id", rec.ID))
	}
	return err
}

// purgeOne purges the deleted backup listed as rec.
func (p purgeRun) purgeOne(ctx context.Context, rec *models.BackupRecord) (PurgeOutcome, error) {
	now, metadataStore, storages, logger := p.now, p.store, p.storages, p.logger
	unlock, err := runs.LockDeletion(ctx, runs.DeletionKey(rec.JobID, rec.ConnectionID, rec.Database))
	if err != nil {
		return PurgeOutcome{}, err
	}
	defer unlock()
	// The grace period in force is read under the lock.
	grace := p.grace()
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
		if refErr == nil && holder == "" && p.locate != nil {
			holder, refErr = physicalHolder(ctx, metadataStore, p.locate, p.holders, current)
		}
		if refErr != nil {
			logger.Warn("purge cannot check for shared archives; keeping the backup deleted", logsafe.Attr("backup_id", current.ID), logsafe.Error(refErr))
			return PurgeOutcome{}, fmt.Errorf("check the archive references of %s: %w", current.ID, refErr)
		}
		if holder != "" {
			out.ArchiveKept = holder
		} else {
			driver, delErr := storages(ctx, current.StorageTargetID)
			var lockedUntil *time.Time
			if delErr == nil {
				// On a locked target every version of the archive is deleted (deleting
				// the key would only add a delete marker), and only once none is
				// locked; other targets delete the key as before.
				lockedUntil, delErr = storage.Purge(ctx, driver, current.StorageKey, current.StorageVersionID, now)
			}
			switch {
			case delErr == nil && lockedUntil != nil:
				// A version is still locked (an archive recorded without its lock, or
				// a lock extended in the bucket): record when it ends and keep the
				// backup deleted until then.
				p.waitForLock(ctx, current.ID, *lockedUntil)
				return PurgeOutcome{}, errPurgeSkip
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
	p.holders.forget(current.ID)
	logger.Info("deleted backup purged after its grace period",
		logsafe.Attr("backup_id", current.ID), logsafe.Attr("database", current.Database),
		slog.Bool("archive_deleted", out.ArchiveDeleted))
	return out, nil
}

// waitForLock records until on deleted backup id: PurgeDue keeps it deleted until
// its archive's lock ends.
func (p purgeRun) waitForLock(ctx context.Context, id string, until time.Time) {
	p.logger.Info("deleted backup waits for the S3 Object Lock of its archive to end",
		logsafe.Attr("backup_id", id), slog.Time("retain_until", until))
	u, ok := p.store.(BackupUpdater)
	if !ok {
		return
	}
	if _, err := u.UpdateBackupRecord(ctx, id, func(r *models.BackupRecord) error {
		at := until.UTC()
		r.RetainUntil = &at
		return nil
	}); err != nil {
		p.logger.Warn("cannot record the lock of a deleted backup's archive", logsafe.Attr("backup_id", id), logsafe.Error(err))
	}
}

// holderIndex maps physical object locations to the live records that name them,
// built once per purge run (see physicalHolder) instead of locating every record
// for every purged backup.
type holderIndex struct {
	built bool
	// byLocation lists the IDs of the records holding each location.
	byLocation map[string][]string
	// unlocated lists the records whose object could not be located: they count as
	// holding every object.
	unlocated []string
	// gone are the records purged by this run since the index was built.
	gone map[string]bool
}

// build fills the index from every live archive reference (once).
func (x *holderIndex) build(ctx context.Context, lister allArchiveRefs, locate LocateFunc) error {
	if x.built {
		return nil
	}
	refs, err := lister.ArchiveRefs(ctx)
	if err != nil {
		return err
	}
	x.byLocation, x.unlocated, x.gone = make(map[string][]string, len(refs)), nil, map[string]bool{}
	for _, r := range refs {
		if !holdsArchive(models.BackupStatus(r.Status)) {
			continue
		}
		loc, locErr := locate(ctx, r.TargetID, r.Key)
		if locErr != nil {
			x.unlocated = append(x.unlocated, r.ID)
			continue
		}
		x.byLocation[loc] = append(x.byLocation[loc], r.ID)
	}
	x.built = true
	return nil
}

// holder returns a record other than self that holds location loc, or "".
func (x *holderIndex) holder(loc, self string) string {
	for _, list := range [][]string{x.byLocation[loc], x.unlocated} {
		for _, id := range list {
			if id != self && !x.gone[id] {
				return id
			}
		}
	}
	return ""
}

// forget drops record id, purged by this run, from the holders.
func (x *holderIndex) forget(id string) {
	if x != nil && x.built {
		x.gone[id] = true
	}
}

// physicalHolder returns the ID of a live record on any storage target whose object
// is the same physical object as rec's (two targets naming one place), or "". An
// object that cannot be located counts as held. index (nil: a fresh one) is built
// on the first call of a run and reused for the others.
func physicalHolder(ctx context.Context, metadataStore store.Store, locate LocateFunc, index *holderIndex, rec *models.BackupRecord) (string, error) {
	lister, ok := metadataStore.(allArchiveRefs)
	if !ok {
		return "", nil
	}
	mine, err := locate(ctx, rec.StorageTargetID, rec.StorageKey)
	if err != nil {
		return "", fmt.Errorf("locate the archive: %w", err)
	}
	if index == nil {
		index = &holderIndex{}
	}
	if err = index.build(ctx, lister, locate); err != nil {
		return "", err
	}
	return index.holder(mine, rec.ID), nil
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
	run := purgeRun{now: s.clock(), grace: s.deleteGrace, store: s.metadataStore, storages: s.storageFor, logger: s.logger, onPurged: s.purged}
	if l, ok := s.targets.(objectLocator); ok {
		run.locate = l.ObjectLocation
	}
	if _, err := run.run(ctx); err != nil {
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
