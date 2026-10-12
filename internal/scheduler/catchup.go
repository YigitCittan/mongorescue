package scheduler

import (
	"context"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// maxCatchUpFires bounds the cron fires CatchUp walks per job (a job every minute
// missed for more than two months stops there).
const maxCatchUpFires = 100_000

// CatchUp runs, once and as a scheduled run, every enabled job whose most recent
// cron fire at or before now was missed: it came after the job's newest scheduled
// run (or its creation), and no backup of the job started at or after it is
// recorded, an imported archive of its database included. A job whose next fire is
// less than half an interval away is left to it. However many fires were missed, a
// job runs at most once. Active-passive HA calls it after a takeover; the scheduler
// must have been started. It returns the IDs of the jobs it started.
func (s *Scheduler) CatchUp(ctx context.Context, now time.Time) []string {
	jobs, err := s.metadataStore.ListJobs(ctx)
	if err != nil {
		s.logger.Warn("missed schedules not checked: cannot list the jobs", logsafe.Error(err))
		return nil
	}
	var started []string
	for _, job := range jobs {
		if !job.Enabled || ctx.Err() != nil {
			continue
		}
		fire, ok := s.missedFire(ctx, job, now)
		if !ok {
			continue
		}
		if !s.startCatchUp(job.ID) {
			continue
		}
		s.logger.Info("running a scheduled backup missed while no instance was active",
			logsafe.Attr("job_id", job.ID), logsafe.Time("missed_fire", &fire))
		started = append(started, job.ID)
	}
	return started
}

// missedFire returns the most recent fire of job at or before now when it was
// missed (see CatchUp).
func (s *Scheduler) missedFire(ctx context.Context, job *models.Job, now time.Time) (time.Time, bool) {
	sched, err := cronParser.Parse(job.CronExpression)
	if err != nil {
		return time.Time{}, false
	}
	from, ok := s.lastScheduledStart(ctx, job.ID)
	if !ok {
		from = job.CreatedAt
	}
	if from.IsZero() || !from.Before(now) {
		return time.Time{}, false
	}
	var fire time.Time
	for t, i := from.In(time.Local), 0; i < maxCatchUpFires; i++ {
		next := sched.Next(t)
		if next.IsZero() || next.After(now) {
			break
		}
		fire, t = next, next
	}
	if fire.IsZero() {
		return time.Time{}, false
	}
	// The regular fire is close: it does the job, and a catch-up run now would
	// make the clock-step guard skip it (see repeatedTrigger).
	if next := sched.Next(now.In(time.Local)); !next.IsZero() && next.Sub(now) < next.Sub(fire)/2 {
		return time.Time{}, false
	}
	if s.coveredSince(ctx, job, fire) {
		return time.Time{}, false
	}
	return fire.UTC(), true
}

// coveredSince reports whether a backup of job started at or after fire is recorded
// and did not fail: one of the job's own, or an imported archive of its database.
func (s *Scheduler) coveredSince(ctx context.Context, job *models.Job, fire time.Time) bool {
	live := func(rows []store.BackupRow) bool {
		for _, row := range rows {
			switch row.Record.Status {
			case models.StatusFailed, models.StatusCancelled:
			default:
				return true
			}
		}
		return false
	}
	page, err := s.metadataStore.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID, From: fire})
	if err != nil || live(page.Rows) {
		// Unknown counts as covered: a duplicate is worse than one missed fire.
		return true
	}
	if job.Database == "" {
		return false
	}
	page, err = s.metadataStore.QueryBackupRecords(ctx, store.BackupFilter{Database: job.Database, From: fire})
	if err != nil {
		return true
	}
	var imported []store.BackupRow
	for _, row := range page.Rows {
		if row.Record.Imported {
			imported = append(imported, row)
		}
	}
	return live(imported)
}

// startCatchUp starts a scheduled run of jobID in the background, tracked like a
// cron fire; it reports false once the scheduler is stopped.
func (s *Scheduler) startCatchUp(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || !s.started {
		return false
	}
	// The run counts as this job's latest trigger for the clock-step guard.
	s.lastTrigger[jobID] = time.Now()
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		s.runScheduled(jobID)
	}()
	return true
}
