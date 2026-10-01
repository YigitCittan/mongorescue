package scheduler

import (
	"errors"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// resumeCheckSchedule is how often paused jobs with a PausedUntil are checked.
const resumeCheckSchedule = "@every 1m"

// errNotDue tells ApplyJobUpdate's persist step that a job changed meanwhile and is
// no longer due to resume.
var errNotDue = errors.New("scheduler: job is not due to resume")

// resumeDueJobs resumes every paused job whose PausedUntil has passed: it is enabled
// again, its PausedUntil cleared and its schedule registered.
func (s *Scheduler) resumeDueJobs() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	ctx := s.ctx
	s.mu.Unlock()

	jobs, err := s.metadataStore.ListJobs(ctx)
	if err != nil {
		s.logger.Warn("cannot check paused jobs", slog.Any("error", err))
		return
	}
	now := time.Now()
	for _, j := range jobs {
		if !dueToResume(j, now) {
			continue
		}
		id := j.ID
		job := j.Clone()
		err := s.ApplyJobUpdate(job, func() error {
			current, getErr := s.metadataStore.GetJob(ctx, id)
			if getErr != nil {
				return getErr
			}
			if !dueToResume(current, now) {
				return errNotDue
			}
			*job = *current.Clone()
			job.Enabled, job.PausedUntil = true, nil
			return s.metadataStore.UpdateJob(ctx, job)
		})
		switch {
		case err == nil:
			s.logger.Info("paused job resumed", slog.String("job_id", id))
		case !errors.Is(err, errNotDue):
			s.logger.Warn("failed to resume a paused job", slog.String("job_id", id), slog.Any("error", err))
		}
	}
}

// dueToResume reports whether job is paused until a time that has passed.
func dueToResume(job *models.Job, now time.Time) bool {
	return !job.Enabled && job.PausedUntil != nil && !job.PausedUntil.After(now)
}
