package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// ErrLockedCopiesRequired is returned (wrapped in ErrInvalid) for a job that does
// not require locked copies while security.require_locked_copies is on: the setting
// applies to every job, and no job can opt out.
var ErrLockedCopiesRequired = errors.New("security.require_locked_copies is on: every job requires locked copies and cannot opt out")

// LockedCopiesHold is a job edit that turned require_locked_copies off: the job is
// stored with it still on, and it is turned off after the grace period (and, with
// the two-person rule, a second administrator's approval), see
// ApplyLockedCopiesHold.
type LockedCopiesHold struct {
	// JobID is the job.
	JobID string
}

// requireLockedCopiesSetting reports whether security.require_locked_copies is on.
func (s *Service) requireLockedCopiesSetting() bool {
	return s.settings().Security.RequireLockedCopies
}

// HoldLockedCopies checks the require_locked_copies of job, about to be stored,
// against existing (nil for a new job) and the security.require_locked_copies
// setting. With the setting on, a new job without it and a job turning it off are
// refused (ErrInvalid wrapping ErrLockedCopiesRequired). With the setting off, a
// job turning it off keeps it on in job and gets a hold, applied with
// ApplyLockedCopiesHold once job is stored; nil means nothing was held.
func (s *Service) HoldLockedCopies(_ context.Context, existing, job *models.Job) (*LockedCopiesHold, error) {
	lowered := existing != nil && existing.RequireLockedCopies && !job.RequireLockedCopies
	if s.requireLockedCopiesSetting() && !job.RequireLockedCopies && (existing == nil || lowered) {
		return nil, invalid(ErrLockedCopiesRequired)
	}
	if !lowered {
		return nil, nil
	}
	job.RequireLockedCopies = true
	return &LockedCopiesHold{JobID: job.ID}, nil
}

// ApplyLockedCopiesHold schedules turning off require_locked_copies of the job of h
// after the grace period or, with the two-person rule, asks a second administrator
// first. It fills the locked copies fields of out; a nil h only reports a change
// already pending.
func (s *Service) ApplyLockedCopiesHold(ctx context.Context, h *LockedCopiesHold, jobID string, out *JobProtection) error {
	if h == nil {
		out.PendingLockedCopies = s.pendingOf(ctx, models.PendingJobLockedCopies, jobID)
		return nil
	}
	job, err := s.store.GetJob(ctx, h.JobID)
	if err != nil {
		return notFound(err, "job not found")
	}
	created := job.CreatedAt
	if s.needsApproval(ctx) {
		a, approvalErr := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalJobLockedCopies, Subject: h.JobID,
			SubjectCreatedAt: &created, Summary: "stop requiring locked copies on job " + h.JobID})
		if approvalErr != nil {
			return approvalErr
		}
		out.LockedCopiesApproval = a
		out.PendingLockedCopies = s.pendingOf(ctx, models.PendingJobLockedCopies, h.JobID)
		return nil
	}
	c, err := s.scheduleJobLockedCopies(ctx, h.JobID, &created)
	if err != nil {
		return err
	}
	out.PendingLockedCopies = c
	return nil
}

// pendingOf returns the pending change of kind for job jobID, or nil.
func (s *Service) pendingOf(ctx context.Context, kind models.PendingChangeKind, jobID string) *models.PendingChange {
	list, err := s.PendingChanges(ctx)
	if err != nil {
		return nil
	}
	for _, c := range list {
		if c.Kind == kind && c.JobID == jobID {
			return c
		}
	}
	return nil
}

// scheduleJobLockedCopies schedules turning off require_locked_copies of job jobID
// (created at bound) after the grace period in force.
func (s *Service) scheduleJobLockedCopies(ctx context.Context, jobID string, bound *time.Time) (*models.PendingChange, error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, notFound(err, "job not found")
	}
	if bound != nil && !job.CreatedAt.Equal(*bound) {
		return nil, public(fmt.Sprintf("job %s was deleted and created again since the request; the request does not apply to the new job", jobID), ErrJobRecreated)
	}
	created := job.CreatedAt
	c, err := s.schedulePending(ctx, &models.PendingChange{Kind: models.PendingJobLockedCopies, JobID: jobID, JobCreatedAt: &created})
	if err != nil {
		return nil, err
	}
	s.destructive(ctx, "disable_job_locked_copies", fmt.Sprintf("job %s stops requiring locked copies at %s", jobID, c.EffectiveAt.Format(time.RFC3339)),
		func(e *events.Event) { e.JobID = jobID })
	return c, nil
}

// applyJobLockedCopiesChange turns off require_locked_copies of the job of c. A job
// deleted and created again since is left alone, and so is every job while
// security.require_locked_copies is on (no job can opt out then).
func (s *Service) applyJobLockedCopiesChange(ctx context.Context, c *models.PendingChange) error {
	existing, err := s.store.GetJob(ctx, c.JobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case c.JobCreatedAt != nil && !existing.CreatedAt.Equal(*c.JobCreatedAt), !existing.RequireLockedCopies:
		return nil
	case s.requireLockedCopiesSetting():
		s.logger.Warn("dropping the pending change that turns off locked copies of a job: security.require_locked_copies is on",
			logsafe.Attr("job_id", c.JobID))
		return nil
	}
	job := existing.Clone()
	persist := func() error {
		current, getErr := s.store.GetJob(ctx, c.JobID)
		if getErr != nil {
			return getErr
		}
		if c.JobCreatedAt != nil && !current.CreatedAt.Equal(*c.JobCreatedAt) {
			return errNotDueYet
		}
		*job = *current.Clone()
		job.RequireLockedCopies = false
		return s.store.UpdateJob(ctx, job)
	}
	if s.cfg.Scheduler != nil {
		err = s.cfg.Scheduler.ApplyJobUpdate(job, persist)
	} else {
		err = persist()
	}
	if err != nil {
		return err
	}
	ctx = withApproval(ctx, &models.Approval{RequestedBy: c.RequestedBy}, c.ApprovedBy)
	s.destructive(ctx, "apply_job_locked_copies", "job "+job.ID+" no longer requires locked copies",
		func(e *events.Event) { e.JobID = job.ID })
	return nil
}
