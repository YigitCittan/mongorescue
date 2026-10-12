package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// Wall clock steps and scheduled jobs.
//
// Job schedules are wall-clock times, so they stay on cron (robfig/cron), which
// sleeps on a monotonic timer until the earliest entry's next time and then runs
// every entry whose next time is not after the wall clock:
//
//   - A forward step (the clock jumps ahead) makes the entries whose times were
//     skipped due when the timer fires: each runs once, late, and is then scheduled
//     after the new time. Missed occurrences are not caught up one by one.
//   - A backward step delays the next trigger by the size of the step: cron waits
//     until the wall clock reaches the entry's next time again. It does not repeat
//     a run by itself, because an entry's next time only moves forward.
//
// A backward step can repeat a run when an entry is rebuilt after it (a job
// edited or re-enabled, or a restart): the rebuilt entry's next time is
// computed from the stepped-back clock and can be a time that already ran. The
// liveness tick is not affected by any of this: it runs on a monotonic ticker.

// cronFire is the cron callback of job jobID: it skips a repeated trigger (see
// repeatedTrigger) and runs the job otherwise.
func (s *Scheduler) cronFire(jobID string) {
	if s.repeatedTrigger(jobID) {
		return
	}
	s.runScheduled(jobID)
}

// repeatedTrigger reports whether a cron trigger of jobID repeats the previous one:
// it came less than half the schedule's interval after it, which only a backward
// clock step makes possible. The previous trigger is the last one this process saw
// (measured on the monotonic clock) or, for the first trigger after a start, the
// start of the job's newest executed scheduled run (wall clock, so a run recorded
// before a backward step looks recent). A skipped trigger is logged; the following
// ones compare with it, so a restart skips at most one.
func (s *Scheduler) repeatedTrigger(jobID string) bool {
	s.mu.Lock()
	var sched cron.Schedule
	if entryID, ok := s.entries[jobID]; ok && !s.stopped {
		sched = s.cron.Entry(entryID).Schedule
	}
	prev, seen := s.lastTrigger[jobID]
	ctx := s.ctx
	s.mu.Unlock()
	if sched == nil {
		return false
	}

	now := time.Now()
	repeated, since := false, time.Duration(0)
	if seen {
		since = now.Sub(prev)
		repeated = since < halfInterval(sched, prev)
	} else if prevStart, ok := s.lastScheduledStart(ctx, jobID); ok {
		since = s.clock().Sub(prevStart)
		repeated = since < halfInterval(sched, prevStart)
	}

	s.mu.Lock()
	if !repeated || !seen {
		s.lastTrigger[jobID] = now
	}
	s.mu.Unlock()
	if repeated {
		s.logger.Warn("skipping a repeated scheduled run: the previous one started less than half the job's interval ago, so the wall clock was set back",
			logsafe.Attr("job_id", jobID), slog.Duration("since_previous", since))
		s.recordNextRun(ctx, &models.Job{ID: jobID}, s.clock())
	}
	return repeated
}

// halfInterval returns half the time from prev to the schedule's next time after
// it: the shortest gap a regular trigger after prev can have is the full interval.
func halfInterval(sched cron.Schedule, prev time.Time) time.Duration {
	return sched.Next(prev).Sub(prev) / 2
}

// lastScheduledStart returns the start of jobID's newest executed scheduled run.
func (s *Scheduler) lastScheduledStart(ctx context.Context, jobID string) (time.Time, bool) {
	list, err := s.metadataStore.ListExecutedJobRuns(ctx, jobID, 10)
	if err != nil {
		return time.Time{}, false
	}
	for _, r := range list {
		if r.Trigger == models.TriggerScheduled && !r.StartedAt.IsZero() {
			return r.StartedAt, true
		}
	}
	return time.Time{}, false
}

// WithTickInterval sets the liveness tick interval (default TickInterval). StaleAfter
// does not change with it; tests use it to see several ticks quickly.
func WithTickInterval(d time.Duration) Option {
	return func(s *Scheduler) { s.tickEvery = d }
}
