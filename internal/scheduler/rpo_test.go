package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// rpoFrom is a mid-January instant: the 8-day window before it crosses no daylight
// saving change in any zone, so the tests hold whatever time.Local is.
var rpoFrom = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

func TestEffectiveRPOBySchedule(t *testing.T) {
	cases := []struct {
		name, cron string
		rpoMinutes int
		want       time.Duration
		isDefault  bool
	}{
		// Two (largest) intervals plus an hour, never less than six hours.
		{"every 15 minutes", "*/15 * * * *", 0, 6 * time.Hour, true},
		{"@every 15m", "@every 15m", 0, 6 * time.Hour, true},
		{"hourly", "@hourly", 0, 6 * time.Hour, true},
		{"@every 2h30m", "@every 2h30m", 0, 6 * time.Hour, true},
		{"every 3h", "0 */3 * * *", 0, 7 * time.Hour, true},
		{"@every 6h", "@every 6h", 0, 13 * time.Hour, true},
		{"daily", "@daily", 0, 49 * time.Hour, true},
		{"daily at 02:00", "0 2 * * *", 0, 49 * time.Hour, true},
		// 09:00 and 17:00: the night is the longest gap (16 hours).
		{"twice a day", "0 9,17 * * *", 0, 33 * time.Hour, true},
		// Weekdays: Friday 09:00 to Monday 09:00 is 72 hours.
		{"weekdays", "0 9 * * 1-5", 0, 145 * time.Hour, true},
		{"weekly", "0 3 * * 0", 0, 337 * time.Hour, true},
		{"@weekly", "@weekly", 0, 337 * time.Hour, true},
		// An unreadable schedule counts as daily.
		{"invalid", "not a cron", 0, 49 * time.Hour, true},
		{"empty", "", 0, 49 * time.Hour, true},
		// A set objective wins over the schedule, also below the default floor.
		{"set on hourly", "@hourly", 90, 90 * time.Minute, false},
		{"set on weekly", "@weekly", 15, 15 * time.Minute, false},
		{"set on invalid", "nope", 60 * 24, 24 * time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job := &models.Job{CronExpression: c.cron, RPOMinutes: c.rpoMinutes}
			got, isDefault := EffectiveRPO(job, rpoFrom)
			if got != c.want || isDefault != c.isDefault {
				t.Fatalf("EffectiveRPO(%q, %d) = %v, %v; want %v, %v", c.cron, c.rpoMinutes, got, isDefault, c.want, c.isDefault)
			}
			if c.isDefault {
				if d := DefaultRPO(c.cron, rpoFrom); d != c.want {
					t.Fatalf("DefaultRPO(%q) = %v; want %v", c.cron, d, c.want)
				}
			}
		})
	}
}

// TestIntervalIsTheSameEveryDay: the interval is the largest gap, so it does not
// change with the day of the week (a weekday schedule is not "daily" on a Tuesday
// and "three days" on a Friday) or the time of day.
func TestIntervalIsTheSameEveryDay(t *testing.T) {
	for _, expr := range []string{"0 9 * * 1-5", "0 9,17 * * *", "@hourly", "@daily", "0 3 * * 0"} {
		first := Interval(expr, rpoFrom)
		for h := 0; h < 7*24; h += 5 {
			if got := Interval(expr, rpoFrom.Add(time.Duration(h)*time.Hour)); got != first {
				t.Fatalf("Interval(%q) at +%dh = %v; want %v", expr, h, got, first)
			}
		}
	}
}

func TestIntervalMonthlyUsesTheLongestMonth(t *testing.T) {
	// A year of month starts: the longest gap is a 31-day month (an hour more or
	// less where daylight saving changes inside it).
	for _, at := range []time.Time{rpoFrom, rpoFrom.AddDate(0, 1, 13), rpoFrom.AddDate(0, 6, 0)} {
		got := Interval("@monthly", at)
		if got < 31*24*time.Hour-time.Hour || got > 31*24*time.Hour+time.Hour {
			t.Fatalf("Interval(@monthly) at %s = %v; want about 31 days", at, got)
		}
	}
}

func TestApplyJobUpdateTellsJobChanged(t *testing.T) {
	st := storetest.New(t)
	var changed []string
	s := NewScheduler(st, nil, storage.NewMockStorage(), nil, WithJobChanged(func(id string) { changed = append(changed, id) }))
	job := &models.Job{ID: "job_a", Name: "a", Database: "shop", CronExpression: "@daily"}
	if err := s.ApplyJobUpdate(job, func() error { return st.SaveJob(context.Background(), job) }); err != nil {
		t.Fatal(err)
	}
	// A failed write tells nobody.
	if err := s.ApplyJobUpdate(job, func() error { return errors.New("store down") }); err == nil {
		t.Fatal("ApplyJobUpdate hid the persist error")
	}
	if len(changed) != 1 || changed[0] != "job_a" {
		t.Fatalf("changed = %v; want [job_a]", changed)
	}
}

func TestInterval(t *testing.T) {
	if got := Interval("@every 6h", rpoFrom); got != 6*time.Hour {
		t.Errorf("Interval(@every 6h) = %v", got)
	}
	if got := Interval("bad", rpoFrom); got != 0 {
		t.Errorf("Interval(bad) = %v; want 0", got)
	}
}
