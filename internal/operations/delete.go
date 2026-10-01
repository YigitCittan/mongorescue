package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// errNoStorage is the cause logged when a backup is deleted without Config.Storage.
var errNoStorage = errors.New("no storage configured")

// archiveErrorText is DeleteResult.ArchiveError when the archive could not be removed.
const archiveErrorText = "the archive could not be deleted from storage (see the server log); the record was removed"

// DeleteResult is the outcome of deleting one backup.
type DeleteResult struct {
	// DeletedID is the deleted backup record.
	DeletedID string `json:"deleted_id"`
	// ArchiveDeleted reports that the archive was removed from storage.
	ArchiveDeleted bool `json:"archive_deleted"`
	// ArchiveKept says why the archive stayed in storage: another record names it.
	ArchiveKept string `json:"archive_kept,omitempty"`
	// ArchiveError says that removing the archive failed; the record is gone anyway,
	// as before, so a broken storage target never blocks cleaning up the history.
	ArchiveError string `json:"archive_error,omitempty"`
}

// archiveRefLister lists the backup rows, readable or not, naming an archive
// (implemented by *store.SQLiteStore).
type archiveRefLister interface {
	ArchiveReferenceIDs(ctx context.Context, targetID, key string) ([]string, error)
}

// archiveRefMapper maps every archive to the rows naming it, in one query
// (implemented by *store.SQLiteStore).
type archiveRefMapper interface {
	ArchiveReferences(ctx context.Context) (map[string][]string, error)
}

// archiveRefs maps archives (see archiveKey) to the IDs of the backup rows naming them.
type archiveRefs map[string][]string

// archiveKey identifies the archive of rec.
func archiveKey(rec *models.BackupRecord) string {
	return rec.StorageTargetID + store.ArchiveKeySep + rec.StorageKey
}

// allArchiveRefs maps every archive to the rows naming it (bulk deletes load it once).
// Without the store's query, only readable rows are counted.
func (s *Service) allArchiveRefs(ctx context.Context) (archiveRefs, error) {
	if m, ok := s.cfg.Store.(archiveRefMapper); ok {
		refs, err := m.ArchiveReferences(ctx)
		if err != nil {
			return nil, err
		}
		return archiveRefs(refs), nil
	}
	all, err := s.cfg.Store.ListBackupRecords(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	refs := archiveRefs{}
	for _, r := range all {
		if r.StorageKey != "" {
			refs[archiveKey(r)] = append(refs[archiveKey(r)], r.ID)
		}
	}
	return refs, nil
}

// archiveRefsOf returns the rows naming the archive of rec (single deletes).
func (s *Service) archiveRefsOf(ctx context.Context, rec *models.BackupRecord) (archiveRefs, error) {
	if rec.StorageKey == "" {
		return archiveRefs{}, nil
	}
	if l, ok := s.cfg.Store.(archiveRefLister); ok {
		ids, err := l.ArchiveReferenceIDs(ctx, rec.StorageTargetID, rec.StorageKey)
		if err != nil {
			return nil, fmt.Errorf("list archive references: %w", err)
		}
		return archiveRefs{archiveKey(rec): ids}, nil
	}
	return s.allArchiveRefs(ctx)
}

// archiveKept returns why the archive of rec must stay when rec is deleted ("" when
// it may go): another row, readable or not, names it. A pinned or unreadable one is
// named, since it holds the archive.
func (s *Service) archiveKept(ctx context.Context, rec *models.BackupRecord, refs archiveRefs) string {
	var others []string
	for _, id := range refs[archiveKey(rec)] {
		if id != rec.ID {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		return ""
	}
	for _, id := range others {
		o, err := s.cfg.Store.GetBackupRecord(ctx, id)
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return fmt.Sprintf("the archive is kept: backup %s, which cannot be read, also names it; only this record was deleted", id)
		case err == nil && o.Pinned:
			return fmt.Sprintf("the archive is kept: it also belongs to backup %s, which is pinned (legal hold); unpin and delete that backup to remove it", id)
		}
	}
	return fmt.Sprintf("the archive is kept: it also belongs to backup %s; only this record was deleted", others[0])
}

// DeleteBackup deletes backup id: its archive on the storage target it was written to
// (unless another record names the same archive, see DeleteResult.ArchiveKept), its
// record and its run log. A pinned backup is refused (ErrPinned): unpin it first. A
// failure to remove the archive is logged and reported in DeleteResult.ArchiveError;
// the record is still deleted. Expected failures: ErrNotFound and ErrPinned.
func (s *Service) DeleteBackup(ctx context.Context, id string) (*DeleteResult, error) {
	rec, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	if err = CheckDeletable(rec); err != nil {
		return nil, err
	}
	refs, err := s.archiveRefsOf(ctx, rec)
	if err != nil {
		return nil, err
	}
	return s.deleteBackup(ctx, rec, refs)
}

// deleteBackup implements DeleteBackup for a loaded, deletable record, with refs
// naming the rows of its archive; refs is updated once the record is gone.
func (s *Service) deleteBackup(ctx context.Context, rec *models.BackupRecord, refs archiveRefs) (*DeleteResult, error) {
	res := &DeleteResult{DeletedID: rec.ID}
	key := archiveKey(rec)
	if rec.StorageKey != "" {
		if res.ArchiveKept = s.archiveKept(ctx, rec, refs); res.ArchiveKept == "" {
			err := s.deleteArchive(ctx, rec)
			switch {
			case err == nil:
				res.ArchiveDeleted = true
			case errors.Is(err, storage.ErrNotFound):
				// Already gone (pruned or removed by hand): nothing to report.
			default:
				s.logger.Error("failed to delete backup artifact from storage",
					logsafe.Attr("backup_id", rec.ID),
					logsafe.Attr("storage_key", rec.StorageKey),
					logsafe.Attr("storage_target_id", rec.StorageTargetID),
					logsafe.Error(err),
				)
				res.ArchiveError = archiveErrorText
			}
		}
	}
	if err := s.cfg.Store.DeleteBackupRecord(ctx, rec.ID); err != nil {
		return nil, notFound(err, "backup not found (deleted meanwhile)")
	}
	s.RemoveRunLog(rec.ID)
	if ids, ok := refs[key]; ok {
		left := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == rec.ID })
		if len(left) == 0 {
			delete(refs, key)
		} else {
			refs[key] = left
		}
	}
	s.logger.Info("backup deleted",
		logsafe.Attr("backup_id", rec.ID),
		logsafe.Attr("database", rec.Database),
		slog.Bool("archive_deleted", res.ArchiveDeleted),
		slog.String("actor", actorName(ctx)),
	)
	return res, nil
}

// deleteArchive removes the archive of rec from its own storage target (not the
// current default).
func (s *Service) deleteArchive(ctx context.Context, rec *models.BackupRecord) error {
	if s.cfg.Storage == nil {
		return errNoStorage
	}
	driver, err := s.cfg.Storage(ctx, rec.StorageTargetID)
	if err != nil {
		return err
	}
	return driver.Delete(ctx, rec.StorageKey)
}

// deleteRestore deletes the history record of a restore. The restored database is
// never touched.
func (s *Service) deleteRestore(ctx context.Context, rec *models.RestoreRecord) error {
	if err := s.cfg.Store.DeleteRestoreRecord(ctx, rec.ID); err != nil {
		return notFound(err, "restore not found (deleted meanwhile)")
	}
	s.logger.Info("restore record deleted",
		logsafe.Attr("restore_id", rec.ID),
		logsafe.Attr("target_database", rec.TargetDatabase),
		slog.String("actor", actorName(ctx)),
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
	if err := s.cfg.Store.DeleteJob(ctx, id); err != nil {
		return notFound(err, "job not found")
	}
	if s.cfg.OnJobDeleted != nil {
		s.cfg.OnJobDeleted(id)
	}
	s.logger.Info("job deleted", logsafe.Attr("job_id", id), slog.String("actor", actorName(ctx)))
	return nil
}

// SetJobEnabled schedules (enabled) or pauses job id and returns the stored job. Only
// the enabled flag changes; the rest is re-read under the scheduler's lock, so a
// concurrent edit is never reverted. Enabling validates the job like UpdateJob (a
// job whose connection is gone cannot be enabled); pausing always works. Expected
// failures: ErrNotFound and those of ValidateJob.
func (s *Service) SetJobEnabled(ctx context.Context, id string, enabled bool) (*models.Job, error) {
	existing, err := s.cfg.Store.GetJob(ctx, id)
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
		current, getErr := s.cfg.Store.GetJob(ctx, id)
		if getErr != nil {
			return notFound(getErr, "job not found")
		}
		*job = *current.Clone()
		job.Enabled = enabled
		job.NextRun = nil
		if enabled && s.cfg.Scheduler == nil {
			job.NextRun = nextRunOf(job.CronExpression, s.now())
		}
		return notFound(s.cfg.Store.UpdateJob(ctx, job), "job not found")
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

// actorName identifies the caller in ctx for logs and bulk summaries by kind and ID
// ("api_key:<id>", "user:<id>", the authentication method, or "system" without a
// principal). Usernames and key names are not logged.
func actorName(ctx context.Context) string {
	p := auth.PrincipalFrom(ctx)
	switch {
	case p == nil:
		return "system"
	case p.APIKeyID != "":
		return "api_key:" + p.APIKeyID
	case p.User != nil:
		return "user:" + p.User.ID
	default:
		return string(p.Method)
	}
}
