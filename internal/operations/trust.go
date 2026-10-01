package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Errors of the trust use cases (pins, retention previews, verification).
var (
	// ErrPinned is returned when deleting a pinned backup; unpin it first.
	ErrPinned = errors.New("operations: the backup is pinned; unpin it before deleting it")
	// ErrUnavailable is returned when a feature's dependency is not configured.
	ErrUnavailable = errors.New("operations: not available")
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
	if s.cfg.Verifier == nil {
		return nil, public("archive verification is not available", ErrUnavailable)
	}
	return s.cfg.Verifier.StartVerify(ctx, id)
}

// PinBackup puts backup id on legal hold with an optional note: retention never
// deletes it, and deleting it needs UnpinBackup first. Pinning a pinned backup
// replaces its note. The pin records who set it (the session's user or the API
// key). Expected failures: ErrNotFound and ErrInvalid.
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
	return s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		r.Pinned, r.PinNote, r.PinnedAt, r.PinnedBy = true, note, &now, by
		return nil
	})
}

// UnpinBackup lifts the pin of backup id, which makes it deletable again; it needs a
// principal with the admin scope in ctx (auth.ErrForbidden otherwise). Expected
// failures: ErrNotFound and auth.ErrForbidden.
func (s *Service) UnpinBackup(ctx context.Context, id string) (*models.BackupRecord, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("lifting a legal hold needs an admin API key or a session: %w", err)
	}
	return s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		r.Pinned, r.PinNote, r.PinnedAt, r.PinnedBy = false, "", nil, ""
		return nil
	})
}

// updateBackup applies fn to backup id atomically.
func (s *Service) updateBackup(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error) {
	u, ok := s.cfg.Store.(backupUpdater)
	if !ok {
		return nil, public("updating backups is not available", ErrUnavailable)
	}
	rec, err := u.UpdateBackupRecord(ctx, id, fn)
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
	records, err := s.cfg.Store.ListBackupRecords(ctx, job.Database)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
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
