package operations

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
)

// referenceSchedule is the schedule part of History before scheduleFigures: every
// job's MaxUpcomingPerJob+1 next runs, all collected and sorted, then cut.
func referenceSchedule(jobs []*models.Job, now time.Time) ([]UpcomingRun, map[string]float64, bool) {
	intervals := map[string]float64{}
	out := []UpcomingRun{}
	truncated := false
	end := now.Add(UpcomingWindow)
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if runs := scheduler.NextRuns(j.CronExpression, now, 2); len(runs) == 2 {
			intervals[j.ID] = runs[1].Sub(runs[0]).Seconds()
		}
		for i, at := range scheduler.NextRuns(j.CronExpression, now, MaxUpcomingPerJob+1) {
			if at.After(end) {
				break
			}
			if i == MaxUpcomingPerJob {
				truncated = true
				break
			}
			out = append(out, UpcomingRun{JobID: j.ID, At: at})
		}
	}
	slices.SortStableFunc(out, compareUpcoming)
	if len(out) > MaxUpcoming {
		out, truncated = out[:MaxUpcoming], true
	}
	return out, intervals, truncated
}

func TestScheduleFiguresMatchesCollectingEveryRun(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 17, 0, 0, time.UTC)
	job := func(id, cron string, enabled bool) *models.Job {
		return &models.Job{ID: id, Name: id, CronExpression: cron, Enabled: enabled}
	}
	many := func(n int, cron func(i int) string) []*models.Job {
		var out []*models.Job
		for i := range n {
			out = append(out, job(fmt.Sprintf("job_%05d", i), cron(i), true))
		}
		return out
	}
	cases := map[string][]*models.Job{
		"none": nil,
		"few": {job("a", "0 3 * * *", true), job("b", "@every 1h", true), job("c", "0 0 1 1 *", true),
			job("d", "*/5 * * * *", false), job("e", "not a schedule", true), job("f", "30 10 * * *", true)},
		"per-job cap": {job("minutely", "* * * * *", true), job("daily", "0 12 * * *", true)},
		"total cap, daily": many(2000, func(i int) string {
			return fmt.Sprintf("%d %d * * *", i%60, (i/60)%24)
		}),
		"total cap, ties": many(800, func(int) string { return "0 * * * *" }),
		"under the cap":   many(300, func(i int) string { return fmt.Sprintf("%d 23 * * *", i%60) }),
	}
	for name, jobs := range cases {
		t.Run(name, func(t *testing.T) {
			got, gotIntervals, gotTrunc := scheduleFigures(jobs, now)
			want, wantIntervals, wantTrunc := referenceSchedule(jobs, now)
			if !reflect.DeepEqual(got, want) || gotTrunc != wantTrunc {
				t.Fatalf("upcoming = %d runs (truncated %v); want %d (truncated %v)", len(got), gotTrunc, len(want), wantTrunc)
			}
			if !reflect.DeepEqual(gotIntervals, wantIntervals) {
				t.Fatalf("intervals = %v; want %v", gotIntervals, wantIntervals)
			}
		})
	}
}

// BenchmarkScheduleFigures measures the schedule part of History at 10,000 daily
// jobs.
func BenchmarkScheduleFigures(b *testing.B) {
	now := time.Date(2026, 10, 12, 10, 17, 0, 0, time.UTC)
	jobs := make([]*models.Job, 10000)
	for i := range jobs {
		jobs[i] = &models.Job{ID: fmt.Sprintf("job_%05d", i), CronExpression: fmt.Sprintf("%d %d * * *", i%60, (i/60)%24), Enabled: true}
	}
	for b.Loop() {
		scheduleFigures(jobs, now)
	}
}
