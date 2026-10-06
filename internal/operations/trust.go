package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Errors of the trust use cases (pins, retention previews, verification).
var (
	// ErrPinned is returned when deleting a pinned backup; unpin it first.
	ErrPinned = errors.New("operations: the backup is pinned; unpin it before deleting it")
	// ErrUnavailable is returned when a feature's dependency is not configured.
	ErrUnavailable = errors.New("operations: not available")
	// ErrLegalHold is returned when the S3 legal hold of a pinned backup's archive
	// (a target with legal_hold_on_pin) could not be set or lifted; the pin is left
	// unchanged.
	ErrLegalHold = errors.New("operations: the S3 legal hold could not be changed")
)

// MaxPinNoteLength bounds the note of a pin, in characters.
const MaxPinNoteLength = 500

// MaxRetentionLogList caps RetentionLog.
const MaxRetentionLogList = 200

// Verifier starts on-demand archive verifications (implemented by
// *integrity.Service).
type Verifier interface {
	// StartVerify verifies backup id in the background and returns its record.
	StartVerify(ctx context.Context, id string) (*models.BackupRecord, error)
}

// backupUpdater updates a backup record atomically (implemented by
// *store.SQLiteStore).
type backupUpdater interface {
	UpdateBackupRecord(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)
}

// retentionLogReader reads the retention log (implemented by *store.SQLiteStore).
type retentionLogReader interface {
	ListRetentionLog(ctx context.Context, jobID string, limit int) ([]*models.RetentionLogEntry, error)
}

// VerifyBackup verifies the archive of backup id in the background; poll the
// backup for verified_at and verification. The integrity service's errors
// (not found, not verifiable, busy) are returned unchanged; without one it returns
// ErrUnavailable.
func (s *Service) VerifyBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	// A caller limited to some connections learns nothing about another one's backup.
	if _, err := s.store.GetBackupRecord(ctx, id); err != nil && auth.ConnectionFilter(ctx).Limited() {
		return nil, notFound(err, "backup not found")
	}
	if s.cfg.Verifier == nil {
		return nil, public("archive verification is not available", ErrUnavailable)
	}
	return s.cfg.Verifier.StartVerify(ctx, id)
}

// PinBackup puts backup id on legal hold with an optional note: retention never
// deletes it, and deleting it needs UnpinBackup first. Pinning a pinned backup
// replaces its note. The pin records who set it (the session's user or the API
// key). On a storage target with legal_hold_on_pin the archive also gets an S3
// legal hold. Expected failures: ErrNotFound, ErrInvalid and ErrLegalHold.
func (s *Service) PinBackup(ctx context.Context, id, note string) (*models.BackupRecord, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxPinNoteLength {
		return nil, public(fmt.Sprintf("note must be at most %d characters", MaxPinNoteLength), ErrInvalid)
	}
	if strings.ContainsFunc(note, func(r rune) bool { return r < 0x20 && r != '\n' || r == 0x7f }) {
		return nil, public("note must not contain control characters", ErrInvalid)
	}
	by := principalName(ctx)
	now := s.now().UTC()
	rec, err := s.store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	// The backup's deletion lock is held across the S3 call and the record update,
	// so a concurrent pin, unpin or deletion never leaves the record and the legal
	// hold in storage disagreeing.
	unlock, err := deletionLock(ctx, rec)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if rec, err = s.store.GetBackupRecord(ctx, id); err != nil {
		return nil, notFound(err, "backup not found")
	}
	// On a target with legal_hold_on_pin the archive gets an S3 legal hold before
	// the pin is recorded: a pin whose hold failed is refused, never half applied.
	hold := false
	if !rec.Status.Deleted() && !rec.LegalHold && s.legalHoldOnPin(ctx, rec) {
		err = s.setLegalHold(ctx, rec, true)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			// No archive to hold (a failed backup, a missing archive): pin only.
		case err != nil:
			return nil, err
		default:
			hold = true
		}
	}
	pinned, err := s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		if r.Status.Deleted() {
			return public(fmt.Sprintf("backup %s is %s; undelete it before pinning it", r.ID, r.Status), ErrBackupDeleted)
		}
		r.Pinned, r.PinNote, r.PinnedAt, r.PinnedBy = true, note, &now, by
		r.LegalHold = r.LegalHold || hold
		return nil
	})
	if err != nil && hold {
		// The pin was not recorded: lift the hold set for it, so it does not keep
		// the archive without a pin that says so.
		if liftErr := s.setLegalHold(context.WithoutCancel(ctx), rec, false); liftErr != nil {
			s.logger.Error("could not lift the legal hold of a pin that failed; the archive stays held without a pin",
				logsafe.Attr("backup_id", rec.ID), logsafe.Error(liftErr))
		}
	}
	return pinned, err
}

// legalHoldOnPin reports whether pinning rec sets an S3 legal hold: its storage
// target locks objects and has legal_hold_on_pin.
func (s *Service) legalHoldOnPin(ctx context.Context, rec *models.BackupRecord) bool {
	if s.cfg.Targets == nil || rec.StorageKey == "" || rec.StorageTargetID == "" {
		return false
	}
	t, err := s.cfg.Targets.Resolve(ctx, rec.StorageTargetID)
	return err == nil && t.ObjectLocked() && t.S3.LegalHoldOnPin
}

// setLegalHold turns the S3 legal hold of rec's archive on or off.
func (s *Service) setLegalHold(ctx context.Context, rec *models.BackupRecord, on bool) error {
	state := map[bool]string{true: "set", false: "lift"}[on]
	if s.cfg.Storage == nil {
		return public(fmt.Sprintf("cannot %s the legal hold of backup %s: storage is not available", state, rec.ID), ErrLegalHold, ErrUnavailable)
	}
	driver, err := s.cfg.Storage(ctx, rec.StorageTargetID)
	if err == nil {
		err = storage.SetLegalHold(ctx, driver, rec.StorageKey, rec.StorageVersionID, on)
	}
	if !on && errors.Is(err, storage.ErrNotFound) {
		// Nothing left to hold.
		return nil
	}
	if err != nil {
		return public(fmt.Sprintf("cannot %s the S3 legal hold of backup %s: %s", state, rec.ID, redact.Text(err.Error())), ErrLegalHold, err)
	}
	return nil
}

// UnpinBackup lifts the pin of backup id, which makes it deletable again; it needs a
// principal with the admin scope in ctx (auth.ErrForbidden otherwise). With the
// two-person rule on, it waits for a second administrator (*ApprovalPendingError).
// An S3 legal hold set by the pin is lifted after the record is unpinned (the pin
// is restored when that fails). Expected failures: ErrNotFound,
// auth.ErrForbidden, ErrApprovalRequired and ErrLegalHold.
func (s *Service) UnpinBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("lifting a legal hold needs the admin role or an admin API key: %w", err)
	}
	if s.needsApproval(ctx) {
		rec, err := s.store.GetBackupRecord(ctx, id)
		if err != nil {
			return nil, notFound(err, "backup not found")
		}
		if !rec.Pinned {
			return rec, nil
		}
		return nil, s.requestApproval(ctx, &models.Approval{Action: models.ApprovalUnpinBackup, Subject: rec.ID,
			Summary: fmt.Sprintf("unpin backup %s (db %s)", rec.ID, rec.Database)})
	}
	rec, err := s.store.GetBackupRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	// Like PinBackup: the deletion lock spans the record update and the S3 call.
	unlock, err := deletionLock(ctx, rec)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if rec, err = s.store.GetBackupRecord(ctx, id); err != nil {
		return nil, notFound(err, "backup not found")
	}
	prev := *rec
	// The record is unpinned first, then the S3 legal hold lifted; if lifting fails
	// the record gets its pin and hold back, so both always agree.
	if rec, err = s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		r.Pinned, r.PinNote, r.PinnedAt, r.PinnedBy = false, "", nil, ""
		r.LegalHold = false
		return nil
	}); err != nil {
		return nil, err
	}
	if prev.LegalHold {
		if holdErr := s.setLegalHold(ctx, &prev, false); holdErr != nil {
			if _, restoreErr := s.updateBackup(context.WithoutCancel(ctx), id, func(r *models.BackupRecord) error {
				r.Pinned, r.PinNote, r.PinnedAt, r.PinnedBy, r.LegalHold = prev.Pinned, prev.PinNote, prev.PinnedAt, prev.PinnedBy, true
				return nil
			}); restoreErr != nil {
				s.logger.Error("could not restore the pin of a backup whose legal hold could not be lifted",
					logsafe.Attr("backup_id", id), logsafe.Error(restoreErr))
			}
			return nil, holdErr
		}
	}
	if prev.Pinned {
		s.destructive(ctx, "unpin_backup", fmt.Sprintf("backup %s (db %s) unpinned", rec.ID, rec.Database),
			func(e *events.Event) { e.BackupID, e.JobID, e.Database = rec.ID, rec.JobID, rec.Database })
	}
	return rec, nil
}

// updateBackup applies fn to backup id atomically.
func (s *Service) updateBackup(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error) {
	u, ok := s.cfg.Store.(backupUpdater)
	if !ok {
		return nil, public("updating backups is not available", ErrUnavailable)
	}
	rec, err := u.UpdateBackupRecord(ctx, id, func(r *models.BackupRecord) error {
		// The updater reads the raw store: apply the caller's connection access here.
		if !backupVisible(ctx, r) {
			return hidden("backup", id)
		}
		return fn(r)
	})
	if err != nil {
		return nil, notFound(err, "backup not found")
	}
	return rec, nil
}

// CheckDeletable returns ErrPinned for a pinned backup (deletion needs an unpin
// first), and nil otherwise.
func CheckDeletable(rec *models.BackupRecord) error {
	if rec != nil && rec.Pinned {
		return public(fmt.Sprintf("backup %s is pinned; unpin it before deleting it", rec.ID), ErrPinned)
	}
	return nil
}

// principalName names the caller in ctx for pins: the user, the API key, or
// "system".
func principalName(ctx context.Context) string {
	p := auth.PrincipalFrom(ctx)
	switch {
	case p == nil:
		return "system"
	case p.User != nil:
		return p.User.Username
	case p.APIKeyName != "":
		return "API key " + p.APIKeyName
	default:
		return string(p.Method)
	}
}

// validateTrust checks a job's verification override and restore test policy; a
// test connection must exist.
func (s *Service) validateTrust(ctx context.Context, job *models.Job) error {
	if !job.VerifyAfterBackup.Valid() {
		return public(`verify_after_backup must be "", "on" or "off"`, ErrInvalid)
	}
	if err := job.RestoreTest.Validate(); err != nil {
		return invalid(err)
	}
	if job.RestoreTest != nil && job.RestoreTest.ConnectionID != "" {
		if _, err := s.ResolveConnection(ctx, job.RestoreTest.ConnectionID); err != nil {
			return fmt.Errorf("restore_test.connection_id: %w", err)
		}
	}
	return nil
}

// RetentionPreview is what a job's retention policy would delete now.
type RetentionPreview struct {
	// JobID is the job.
	JobID string `json:"job_id"`
	// RetentionDays and RetentionCount are the policy previewed (the job's own, or
	// the values asked for).
	RetentionDays  int `json:"retention_days"`
	RetentionCount int `json:"retention_count"`
	// At is the time the preview applies the policy at.
	At time.Time `json:"at"`
	// The plan: the backups deleted (oldest first) and the ones kept although a rule
	// selects them (pinned, last verified).
	scheduler.RetentionPlan
}

// RetentionPreview returns the backups job jobID's retention policy would delete if
// it ran now, and why, without deleting anything. days and count, when set, preview
// those values instead of the job's (e.g. while the job is edited); they must not
// be negative. The next scheduled run applies the same rules after adding its new
// backup, so a count policy then deletes one more. Expected failures: ErrNotFound
// and ErrInvalid.
func (s *Service) RetentionPreview(ctx context.Context, jobID string, days, count *int) (*RetentionPreview, error) {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	d, c := derefOr(days, job.RetentionDays), derefOr(count, job.RetentionCount)
	if d < 0 || c < 0 {
		return nil, invalid(ErrNegativeRetention)
	}
	// Every database of the job: the policy applies to each of them separately.
	page, err := s.store.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID})
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	records := make([]*models.BackupRecord, 0, len(page.Rows))
	for _, row := range page.Rows {
		records = append(records, row.Record)
	}
	// The target is resolved like a scheduled run resolves it, so a legacy job
	// without a storage_target_id previews its default target's backups.
	target, err := s.ResolveTarget(ctx, job.StorageTargetID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	plan := scheduler.PlanRetention(now, d, c, scheduler.JobRetentionHistory(job, target.ID, records))
	return &RetentionPreview{JobID: job.ID, RetentionDays: d, RetentionCount: c, At: now, RetentionPlan: plan}, nil
}

// RetentionLog returns up to limit (at most MaxRetentionLogList) backups job jobID's
// retention deleted, newest first. Expected failures: ErrNotFound.
func (s *Service) RetentionLog(ctx context.Context, jobID string, limit int) ([]*models.RetentionLogEntry, error) {
	if _, err := s.GetJob(ctx, jobID); err != nil {
		return nil, err
	}
	r, ok := s.cfg.Store.(retentionLogReader)
	if !ok {
		return []*models.RetentionLogEntry{}, nil
	}
	if limit <= 0 || limit > MaxRetentionLogList {
		limit = MaxRetentionLogList
	}
	list, err := r.ListRetentionLog(ctx, jobID, limit)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("list retention log: %w", err)
	}
	return list, nil
}
