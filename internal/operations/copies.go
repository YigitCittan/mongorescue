package operations

import (
	"context"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// RetryCopies queues every failed copy of completed backup id again, with its
// attempts reset (also copies the queue gave up on), and wakes the copy queue.
// Admin only. Expected failures: ErrNotFound and ErrInvalid (the backup is not
// completed or has no failed copy).
func (s *Service) RetryCopies(ctx context.Context, id string) (*models.BackupRecord, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("retrying copies needs the admin role or an admin API key: %w", err)
	}
	rec, err := s.updateBackup(ctx, id, func(r *models.BackupRecord) error {
		switch {
		case r.Status != models.StatusCompleted:
			return public(fmt.Sprintf("only the copies of completed backups can be retried; backup %s is %s", r.ID, r.Status), ErrInvalid)
		case !r.RetryCopies():
			return public(fmt.Sprintf("backup %s has no failed copy", r.ID), ErrInvalid)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.cfg.WakeCopies != nil {
		s.cfg.WakeCopies()
	}
	return rec, nil
}

// resolveCopyTargets checks the copy targets ids of a job or backup whose primary
// target is primary and returns them resolved, in order: mode must be a known copy
// mode, at most models.MaxCopyTargets distinct targets other than primary, each one
// a storage target the caller sees (a caller limited to some connections may only
// name the targets it sees, as for primaries). Expected failures: ErrInvalid and
// ErrUnknownStorageTarget.
func (s *Service) resolveCopyTargets(ctx context.Context, ids []string, primary string, mode models.CopyMode) ([]models.CopyTarget, error) {
	if !mode.Valid() {
		return nil, invalid(fmt.Errorf("%w: copy_mode must be %q or %q", models.ErrInvalidCopyTargets, models.CopyAsync, models.CopySync))
	}
	ids, err := models.NormalizeCopyTargets(ids, primary)
	if err != nil {
		return nil, invalid(err)
	}
	out := make([]models.CopyTarget, 0, len(ids))
	for _, id := range ids {
		t, resolveErr := s.ResolveTarget(ctx, id)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if t.ID == primary {
			return nil, invalid(fmt.Errorf("%w: the primary storage target cannot also be a copy target", models.ErrInvalidCopyTargets))
		}
		out = append(out, models.CopyTarget{ID: t.ID, Name: t.Name})
	}
	return out, nil
}

// checkLockedCopies refuses copy targets without S3 Object Lock for a job that
// requires locked copies (models.Job.RequireLockedCopies). Expected failures:
// ErrInvalid (wrapping models.ErrUnlockedCopyTarget) and ErrUnknownStorageTarget.
func (s *Service) checkLockedCopies(ctx context.Context, copies []models.CopyTarget) error {
	for _, c := range copies {
		t, err := s.ResolveTarget(ctx, c.ID)
		if err != nil {
			return err
		}
		if !t.ObjectLocked() {
			return invalid(fmt.Errorf("%w: copy target %s has no S3 Object Lock (set an object lock mode on it, or turn require_locked_copies off)",
				models.ErrUnlockedCopyTarget, targetLabel(t.Name, t.ID)))
		}
	}
	return nil
}
