package scheduler

import (
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Interval returns the gap between the next two activations of expr after from, or
// 0 when expr cannot be parsed or has fewer than two activations left.
func Interval(expr string, from time.Time) time.Duration {
	runs := NextRuns(expr, from, 2)
	if len(runs) < 2 {
		return 0
	}
	return runs[1].Sub(runs[0])
}

// DefaultRPO returns the recovery point objective of a job scheduled with expr that
// sets none: two schedule intervals plus an hour, at least six hours (see
// models.DefaultRPO). An unreadable schedule counts as daily.
func DefaultRPO(expr string, from time.Time) time.Duration {
	return models.DefaultRPO(Interval(expr, from))
}

// EffectiveRPO returns job's recovery point objective at from: its RPOMinutes when
// set, else DefaultRPO of its schedule. The second result reports whether the
// default applies.
func EffectiveRPO(job *models.Job, from time.Time) (time.Duration, bool) {
	if job.RPOMinutes > 0 {
		return job.RPO(0)
	}
	return job.RPO(Interval(job.CronExpression, from))
}
