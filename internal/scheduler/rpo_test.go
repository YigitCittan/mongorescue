package scheduler

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestEffectiveRPOBySchedule(t *testing.T) {
	// A UTC instant away from month ends: schedules are read in from's zone.
	from := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	cases := []struct {
		name, cron string
		rpoMinutes int
		want       time.Duration
		isDefault  bool
	}{
		// Two intervals plus an hour, never less than six hours.
		{"every 15 minutes", "*/15 * * * *", 0, 6 * time.Hour, true},
		{"@every 15m", "@every 15m", 0, 6 * time.Hour, true},
		{"hourly", "@hourly", 0, 6 * time.Hour, true},
		{"every 2h30m", "@every 2h30m", 0, 6 * time.Hour, true},
		{"every 3h", "0 */3 * * *", 0, 7 * time.Hour, true},
		{"@every 6h", "@every 6h", 0, 13 * time.Hour, true},
		{"daily", "@daily", 0, 49 * time.Hour, true},
		{"daily at 02:00", "0 2 * * *", 0, 49 * time.Hour, true},
		{"weekly", "0 3 * * 0", 0, 337 * time.Hour, true},
		{"@weekly", "@weekly", 0, 337 * time.Hour, true},
		// February 1 to March 1: 28 days.
		{"monthly", "@monthly", 0, 2*28*24*time.Hour + time.Hour, true},
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
			got, isDefault := EffectiveRPO(job, from)
			if got != c.want || isDefault != c.isDefault {
				t.Fatalf("EffectiveRPO(%q, %d) = %v, %v; want %v, %v", c.cron, c.rpoMinutes, got, isDefault, c.want, c.isDefault)
			}
			if c.isDefault {
				if d := DefaultRPO(c.cron, from); d != c.want {
					t.Fatalf("DefaultRPO(%q) = %v; want %v", c.cron, d, c.want)
				}
			}
		})
	}
}

func TestInterval(t *testing.T) {
	from := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	if got := Interval("@every 6h", from); got != 6*time.Hour {
		t.Errorf("Interval(@every 6h) = %v", got)
	}
	if got := Interval("bad", from); got != 0 {
		t.Errorf("Interval(bad) = %v; want 0", got)
	}
}
