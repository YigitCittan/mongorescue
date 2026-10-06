package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// targetUpdater updates storage targets and applies held lock lowerings
// (implemented by *targets.Service).
type targetUpdater interface {
	Update(ctx context.Context, id string, in targets.Input) (*models.StorageTarget, error)
	LowerObjectLock(ctx context.Context, id string, lowered models.ObjectLockSettings, createdAt *time.Time) (bool, error)
}

// TargetSaveResult is an updated storage target and what its edit deferred: a
// lowered S3 Object Lock waits for the delete grace period (and, with the
// two-person rule, for a second administrator first).
type TargetSaveResult struct {
	*models.StorageTarget
	// PendingObjectLock is the scheduled lowering of the target's object lock.
	PendingObjectLock *models.PendingChange `json:"pending_object_lock,omitempty"`
	// Approval is the request for a second administrator, if the lowering waits
	// for one.
	Approval *models.Approval `json:"approval,omitempty"`
}

// UpdateTarget updates storage target id like targets.Service.Update, except that
// lowering its S3 Object Lock follows delete protection: a weaker mode, no lock, a
// shorter retention or the legal hold on pin turned off is stored only after the
// delete grace period, as a pending change (with the two-person rule, after a
// second administrator approved it), while the target keeps its current lock
// meanwhile. Raising the lock applies at once, and an edit that changes the lock
// without lowering it drops a pending lowering. Expected failures: those of
// targets.Service.Update.
func (s *Service) UpdateTarget(ctx context.Context, id string, in targets.Input) (*TargetSaveResult, error) {
	u, ok := s.cfg.Targets.(targetUpdater)
	if !ok {
		return nil, public("storage targets are not configured", ErrUnavailable)
	}
	existing, err := s.cfg.Targets.Resolve(ctx, id)
	if err != nil || id == "" {
		if err == nil {
			err = targets.ErrNotFound
		}
		return nil, err
	}
	var held *models.ObjectLockSettings
	if existing.Type == models.StorageS3 && existing.S3 != nil && in.Type == models.StorageS3 && in.S3 != nil {
		requested := models.ObjectLockSettings{Mode: in.S3.ObjectLock, RetentionDays: in.S3.RetentionDays, LegalHoldOnPin: in.S3.LegalHoldOnPin}
		if requested.Valid() {
			now, h := models.SplitLockChange(existing.S3.LockSettings(), requested)
			s3 := *in.S3
			s3.SetLockSettings(now)
			in.S3, held = &s3, h
		}
	}
	t, err := u.Update(ctx, id, in)
	if err != nil {
		return nil, err
	}
	out := &TargetSaveResult{StorageTarget: t}
	changed := t.S3 != nil && existing.S3 != nil && t.S3.LockSettings() != existing.S3.LockSettings()
	if held == nil {
		if changed {
			if st, stOK := s.cfg.Store.(pendingStore); stOK {
				if _, err = st.DeletePendingChangesOf(ctx, models.PendingObjectLock, id); err != nil {
					return nil, fmt.Errorf("cancel the pending object lock change: %w", err)
				}
			}
		}
		out.PendingObjectLock = s.pendingObjectLock(ctx, id)
		return out, nil
	}
	created := existing.CreatedAt
	if s.needsApproval(ctx) {
		a, approvalErr := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalLowerObjectLock, Subject: id,
			ObjectLock: held, SubjectCreatedAt: &created,
			Summary: fmt.Sprintf("lower the object lock of storage target %s (%s) from %s to %s", existing.Name, id, existing.S3.LockSettings(), held)})
		if approvalErr != nil {
			return nil, approvalErr
		}
		out.Approval = a
		out.PendingObjectLock = s.pendingObjectLock(ctx, id)
		return out, nil
	}
	if out.PendingObjectLock, err = s.scheduleObjectLock(ctx, id, *held, &created); err != nil {
		return nil, err
	}
	return out, nil
}

// pendingObjectLock returns the pending object lock change of target id, or nil.
func (s *Service) pendingObjectLock(ctx context.Context, id string) *models.PendingChange {
	list, err := s.PendingChanges(ctx)
	if err != nil {
		return nil
	}
	for _, c := range list {
		if c.Kind == models.PendingObjectLock && c.TargetID == id {
			return c
		}
	}
	return nil
}

// scheduleObjectLock schedules lowering the object lock of target id to lock after
// the grace period. bound, when set, is the creation time of the target the request
// was made for: a target deleted and created again since is refused.
func (s *Service) scheduleObjectLock(ctx context.Context, id string, lock models.ObjectLockSettings, bound *time.Time) (*models.PendingChange, error) {
	t, err := s.cfg.Targets.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	if bound != nil && !t.CreatedAt.Equal(*bound) {
		return nil, public(fmt.Sprintf("storage target %s was deleted and created again since the request; the request does not apply to the new target", id), ErrInvalid)
	}
	created := t.CreatedAt
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingObjectLock, TargetID: id, TargetCreatedAt: &created, ObjectLock: &lock})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "lower_object_lock", fmt.Sprintf("object lock of storage target %s lowered to %s, effective %s", id, lock, c.EffectiveAt.Format(time.RFC3339)),
		func(e *events.Event) { e.TargetID = id })
	return c, nil
}

// applyObjectLockChange lowers the object lock of the target of c; each part of the
// lock becomes the weaker of the lock in force and c's (models.LowerLock).
func (s *Service) applyObjectLockChange(ctx context.Context, c *models.PendingChange) error {
	u, ok := s.cfg.Targets.(targetUpdater)
	if !ok || c.ObjectLock == nil {
		return nil
	}
	changed, err := u.LowerObjectLock(ctx, c.TargetID, *c.ObjectLock, c.TargetCreatedAt)
	if err != nil || !changed {
		if errors.Is(err, targets.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_object_lock", fmt.Sprintf("object lock of storage target %s lowered to %s", c.TargetID, c.ObjectLock),
		func(e *events.Event) { e.TargetID = c.TargetID })
	return nil
}
