package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// WindowEndReason is the cancellation reason of a scheduled run still running when
// its job's backup window closed (cancel_at_window_end).
const WindowEndReason = "the backup window ended"

// checkWindow applies job's backup window to a scheduled run firing now. It
// reports false when the run must not start, after recording and publishing it
// as skipped (outside window); otherwise it returns a stop function (never nil)
// for the cancellation at the window's end, armed only with
// cancel_at_window_end.
func (s *Scheduler) checkWindow(ctx context.Context, job *models.Job) (stop func(), start bool) {
	w := job.BackupWindow
	if w == nil {
		return func() {}, true
	}
	now := s.clock()
	_, end, open := w.Open(now)
	if !open {
		s.skipRun(ctx, job, models.SkipOutsideWindow, now)
		return nil, false
	}
	if !w.CancelAtWindowEnd {
		return func() {}, true
	}
	timer := time.AfterFunc(end.Sub(now), func() {
		ids := s.registry.CancelJob(job.ID, runs.Cancellation{By: runs.SystemActor, Kind: runs.ActorSystem, Reason: WindowEndReason, At: s.clock().UTC()})
		if len(ids) > 0 {
			s.logger.Warn("cancelled a scheduled backup at the end of its window",
				logsafe.Attr("job_id", job.ID), slog.Any("backup_ids", ids), slog.String("window", w.String()))
		}
	})
	return func() { timer.Stop() }, true
}

// skipRun records a scheduled run of job that did not start for reason and
// refreshes the job's next run (its last run stays). A skipped run is neither a
// success nor a failure.
//
// Skips coalesce: while the job's newest run is a skip for the same reason (no
// run happened since), that record counts one more skipped activation
// (JobRun.SkippedRuns) instead of a new record being written, so an hourly job
// with a four-hour window writes one record per day, not twenty. Only the first
// skip of such a gap publishes events.BackupSkipped.
func (s *Scheduler) skipRun(ctx context.Context, job *models.Job, reason string, at time.Time) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	s.recordNextRun(persistCtx, job, at)
	s.logger.Info("skipping scheduled backup: outside the job's backup window",
		logsafe.Attr("job_id", job.ID), slog.String("window", job.BackupWindow.String()))
	latest, err := s.metadataStore.ListJobRuns(persistCtx, job.ID, 1)
	if err == nil && len(latest) == 1 && latest[0].Status == models.JobRunSkipped && latest[0].SkipReason == reason {
		latest[0].Skip(reason, at)
		s.saveRun(persistCtx, latest[0])
		return
	}
	run := s.newScheduledRun(job)
	run.StartedAt = at.UTC()
	run.Skip(reason, at)
	s.saveRun(persistCtx, run)
	if s.publisher != nil {
		db := ""
		if !job.MultiDatabase() {
			db = job.Database
		}
		s.publisher.Publish(persistCtx, events.SkippedRunEvent(run, db))
	}
}

// newScheduledRun returns the summary of a scheduled run of job starting now by
// the scheduler's clock, the clock its backup window is read with, so skipped and
// executed runs sort in the order they happened.
func (s *Scheduler) newScheduledRun(job *models.Job) *models.JobRun {
	run := newRun(job, models.TriggerScheduled)
	run.StartedAt = s.clock().UTC()
	return run
}

// recordNextRun stores job's next run after at, keeping its last run.
func (s *Scheduler) recordNextRun(ctx context.Context, job *models.Job, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entryID, exists := s.entries[job.ID]
	if !exists {
		return
	}
	sched := s.cron.Entry(entryID).Schedule
	if sched == nil {
		return
	}
	next := sched.Next(at).UTC()
	if err := s.metadataStore.UpdateJobRunTimes(ctx, job.ID, nil, &next); err != nil {
		s.logger.Warn("failed to persist the next run of a skipped job", logsafe.Attr("job_id", job.ID), slog.Any("error", err))
	}
	job.NextRun = &next
}
