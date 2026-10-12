package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// maxCatchUpFires bounds the cron fires CatchUp walks per job (a job every minute
// missed for more than two months stops there).
const maxCatchUpFires = 100_000

// ImportCheck reports whether the archives written to storage target targetID ("":
// the default target) since the last metadata snapshot are known: the target was
// scanned and its newer archives imported (nil), or why that is not certain.
type ImportCheck func(ctx context.Context, targetID string) error

// errNoImportCheck is the reason of a catch-up without an ImportCheck.
var errNoImportCheck = errors.New("the archives written during the gap were not checked")

// CatchUp runs, once and as a scheduled run, every enabled job whose most recent
// cron fire at or before now was missed: it came after the job's newest scheduled
// run (or its creation), and no backup of the job started at or after it is
// recorded, an imported archive of its databases included. A job whose next fire is
// less than half an interval away is left to it. However many fires were missed, a
// job runs at most once. Active-passive HA calls it after a takeover; the scheduler
// must have been started. It returns the IDs of the jobs it started.
//
// A missed fire runs only when it is certain that no backup was taken for it:
// imports must report the job's storage target checked, and the job's databases
// must be known. Otherwise the fire is not caught up (a duplicate is worse than a
// missed fire) and an events.HACatchUpSkipped event says so.
func (s *Scheduler) CatchUp(ctx context.Context, now time.Time, imports ImportCheck) []string {
	if !s.fenceAllows(ctx, "missed-schedule catch-up") {
		return nil
	}
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
		if reason := s.unverifiable(ctx, job, imports); reason != nil {
			s.catchUpSkipped(ctx, job, fire, reason)
			continue
		}
		if s.coveredSince(ctx, job, fire) {
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

// unverifiable returns why it is not certain that job took no backup during the
// gap, or nil.
func (s *Scheduler) unverifiable(ctx context.Context, job *models.Job, imports ImportCheck) error {
	if imports == nil {
		return errNoImportCheck
	}
	if err := imports(ctx, job.StorageTargetID); err != nil {
		return err
	}
	if len(jobDatabases(job)) == 0 {
		return errors.New("the job's databases are not known, so its imported archives cannot be matched")
	}
	return nil
}

// catchUpSkipped reports a missed fire that is not caught up.
func (s *Scheduler) catchUpSkipped(ctx context.Context, job *models.Job, fire time.Time, reason error) {
	detail := fmt.Sprintf("the fire of job %s at %s was missed while no instance was active and is not caught up: could not verify that it did not run (%v); run the job by hand if it did not",
		job.ID, fire.UTC().Format(time.RFC3339), reason)
	s.logger.Warn("missed fire not caught up: could not verify", logsafe.Attr("job_id", job.ID),
		logsafe.Time("missed_fire", &fire), logsafe.Error(reason))
	if s.publisher != nil {
		s.publisher.Publish(ctx, events.Event{Type: events.HACatchUpSkipped, Time: time.Now().UTC(), JobID: job.ID,
			Database: job.Database, Status: "skipped", Detail: detail})
	}
}

// jobDatabases returns the databases job backs up, as far as they are known.
func jobDatabases(job *models.Job) []string {
	if job.Database != "" {
		return []string{job.Database}
	}
	return job.KnownDatabases
}

// missedFire returns the most recent fire of job at or before now when it was
// missed (see CatchUp): after its newest scheduled run, and not about to be
// followed by the next regular fire.
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
	return fire.UTC(), true
}

// coveredSince reports whether a backup of job started at or after fire is recorded
// and did not fail: one of the job's own, or an imported archive of one of its
// databases. A lookup that fails counts as covered.
func (s *Scheduler) coveredSince(ctx context.Context, job *models.Job, fire time.Time) bool {
	live := func(rows []store.BackupRow, match func(*models.BackupRecord) bool) bool {
		for _, row := range rows {
			switch row.Record.Status {
			case models.StatusFailed, models.StatusCancelled:
			default:
				if match(row.Record) {
					return true
				}
			}
		}
		return false
	}
	page, err := s.metadataStore.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID, From: fire})
	if err != nil || live(page.Rows, func(*models.BackupRecord) bool { return true }) {
		// Unknown counts as covered: a duplicate is worse than one missed fire.
		return true
	}
	dbs := jobDatabases(job)
	page, err = s.metadataStore.QueryBackupRecords(ctx, store.BackupFilter{From: fire})
	if err != nil {
		return true
	}
	return live(page.Rows, func(r *models.BackupRecord) bool { return r.Imported && slices.Contains(dbs, r.Database) })
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
