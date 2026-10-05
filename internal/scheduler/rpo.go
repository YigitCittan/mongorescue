package scheduler

import (
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Bounds of Interval.
const (
	// intervalWindow is the span whose activations Interval compares: more than a
	// week, so weekday and weekend patterns are always covered.
	intervalWindow = 8 * 24 * time.Hour
	// intervalLongWindow is the span used instead when the gaps are longer than
	// intervalWindow (monthly and rarer schedules), so the longest month counts.
	intervalLongWindow = 366 * 24 * time.Hour
	// maxIntervalRuns bounds the activations Interval reads (8 days of a schedule
	// that runs every minute are 11520).
	maxIntervalRuns = 20000
)

// Interval returns the largest gap between consecutive activations of expr in the
// 8 days before from (a year for schedules whose gaps are longer than that), read
// in the scheduler's time zone, or 0 when expr cannot be parsed or has fewer than
// two activations. The largest gap, not the next one, makes the result the same
// on every day of the week: "0 9 * * 1-5" is 72 hours (Friday to Monday) also on a
// Tuesday, and "0 9,17 * * *" is 16 hours also at noon.
func Interval(expr string, from time.Time) time.Duration {
	return WindowedInterval(expr, nil, from)
}

// WindowedInterval is Interval counting only the activations window lets start
// (see models.BackupWindow.Contains; a nil window allows all): the runs a window
// skips do not back anything up, so the largest gap between allowed runs is the
// interval. A schedule whose activations all fall outside the window has none (0).
func WindowedInterval(expr string, window *models.BackupWindow, from time.Time) time.Duration {
	schedule, err := cronParser.Parse(expr)
	if err != nil {
		return 0
	}
	// The scheduler's cron runs in time.Local; schedules without a zone of their
	// own are read in the zone of the time they are asked about.
	start := from.In(time.Local).Add(-intervalWindow)
	end := start.Add(intervalWindow)
	// next returns the first allowed activation after t (zero for none within
	// maxIntervalRuns activations).
	reads := 0
	next := func(t time.Time) time.Time {
		for reads < maxIntervalRuns {
			reads++
			t = schedule.Next(t)
			if t.IsZero() || window.Contains(t) {
				return t
			}
		}
		return time.Time{}
	}
	prev := next(start)
	if prev.IsZero() {
		return 0
	}
	var gap time.Duration
	for reads < maxIntervalRuns {
		n := next(prev)
		if n.IsZero() {
			break
		}
		gap = max(gap, n.Sub(prev))
		if n.After(end) {
			if gap <= intervalWindow || end.Sub(start) >= intervalLongWindow {
				break
			}
			end = start.Add(intervalLongWindow)
		}
		prev = n
	}
	return gap
}

// DefaultRPO returns the recovery point objective of a job scheduled with expr that
// sets none: two schedule intervals (see Interval) plus an hour, at least six hours
// (see models.DefaultRPO). An unreadable schedule counts as daily.
func DefaultRPO(expr string, from time.Time) time.Duration {
	return models.DefaultRPO(Interval(expr, from))
}

// EffectiveRPO returns job's recovery point objective at from: its RPOMinutes when
// set, else DefaultRPO of its schedule. The second result reports whether the
// default applies.
//
// A job with a backup window counts only the runs the window allows (see
// WindowedInterval): a schedule every hour with a window of 01:00 to 05:00 is a
// 21-hour interval, not one hour.
func EffectiveRPO(job *models.Job, from time.Time) (time.Duration, bool) {
	if job.RPOMinutes > 0 {
		return job.RPO(0)
	}
	return job.RPO(WindowedInterval(job.CronExpression, job.BackupWindow, from))
}

// WithJobChanged runs fn with a job's ID after every job write that went through
// ApplyJobUpdate (creates, edits, pauses, resumes): the RPO checker re-evaluates
// it at once. fn must not block.
func WithJobChanged(fn func(jobID string)) Option {
	return func(s *Scheduler) { s.jobChanged = fn }
}
