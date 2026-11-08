package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Bounds of the grandfather-father-son (GFS) retention buckets of a RetentionPolicy.
const (
	// MaxKeepDaily is the largest keep_daily (a little over a year of days).
	MaxKeepDaily = 400
	// MaxKeepWeekly is the largest keep_weekly (ten years of weeks).
	MaxKeepWeekly = 520
	// MaxKeepMonthly is the largest keep_monthly (twenty years of months).
	MaxKeepMonthly = 240
	// MaxKeepYearly is the largest keep_yearly.
	MaxKeepYearly = 100
)

// ErrInvalidRetention is returned for a retention policy with a negative or out of
// range value, or a copy retention override that sets nothing or names a target the
// job does not copy to. The operations layer maps it to ErrInvalid.
var ErrInvalidRetention = errors.New("invalid retention policy")

// RetentionPolicy says how long backups are kept. Every field is optional and 0 turns
// its rule off. A backup is kept as long as at least one rule keeps it (see the
// retention documentation for the precise union).
type RetentionPolicy struct {
	// RetentionDays keeps backups for this many days (0 = no age limit).
	RetentionDays int `json:"retention_days,omitempty"`
	// RetentionCount keeps the newest N backups of each database (0 = no count limit).
	RetentionCount int `json:"retention_count,omitempty"`
	// KeepDaily keeps the newest backup of each of the last N days that have one.
	KeepDaily int `json:"keep_daily,omitempty"`
	// KeepWeekly keeps the newest backup of each of the last N ISO weeks that have one.
	KeepWeekly int `json:"keep_weekly,omitempty"`
	// KeepMonthly keeps the newest backup of each of the last N months that have one.
	KeepMonthly int `json:"keep_monthly,omitempty"`
	// KeepYearly keeps the newest backup of each of the last N years that have one.
	KeepYearly int `json:"keep_yearly,omitempty"`
}

// GFS reports whether any bucket rule (keep_daily to keep_yearly) is on.
func (p RetentionPolicy) GFS() bool {
	return p.KeepDaily > 0 || p.KeepWeekly > 0 || p.KeepMonthly > 0 || p.KeepYearly > 0
}

// Active reports whether any rule is on.
func (p RetentionPolicy) Active() bool {
	return p.RetentionDays > 0 || p.RetentionCount > 0 || p.GFS()
}

// Validate checks that no value is negative and the buckets are within their bounds.
// It wraps ErrInvalidRetention.
func (p RetentionPolicy) Validate() error {
	for _, f := range []struct {
		name     string
		v, limit int
	}{
		{"retention_days", p.RetentionDays, 0}, {"retention_count", p.RetentionCount, 0},
		{"keep_daily", p.KeepDaily, MaxKeepDaily}, {"keep_weekly", p.KeepWeekly, MaxKeepWeekly},
		{"keep_monthly", p.KeepMonthly, MaxKeepMonthly}, {"keep_yearly", p.KeepYearly, MaxKeepYearly},
	} {
		switch {
		case f.v < 0:
			return fmt.Errorf("%w: %s must not be negative", ErrInvalidRetention, f.name)
		case f.limit > 0 && f.v > f.limit:
			return fmt.Errorf("%w: %s must be at most %d", ErrInvalidRetention, f.name, f.limit)
		}
	}
	return nil
}

// RetentionPolicy returns the job's own retention rules.
func (j *Job) RetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		RetentionDays: j.RetentionDays, RetentionCount: j.RetentionCount,
		KeepDaily: j.KeepDaily, KeepWeekly: j.KeepWeekly, KeepMonthly: j.KeepMonthly, KeepYearly: j.KeepYearly,
	}
}

// ValidateCopyRetention checks the per copy target retention overrides of a job:
// every key is one of copyTargets, and every override sets at least one valid rule.
// It wraps ErrInvalidRetention.
func ValidateCopyRetention(overrides map[string]RetentionPolicy, copyTargets []string) error {
	for id, p := range overrides {
		switch {
		case !containsString(copyTargets, id):
			return fmt.Errorf("%w: copy_retention names %q, which is not one of the job's copy targets", ErrInvalidRetention, id)
		case !p.Active():
			return fmt.Errorf("%w: the copy_retention of %q sets no rule", ErrInvalidRetention, id)
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("copy_retention of %q: %w", id, err)
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RetentionLocation returns the time zone retention buckets of the job's backups are
// cut in: the zone of the job's schedule (a "CRON_TZ=Zone" or "TZ=Zone" prefix of
// cron_expression), UTC when the schedule names none or an unknown one.
func (j *Job) RetentionLocation() *time.Location {
	s := strings.TrimSpace(j.CronExpression)
	for _, prefix := range []string{"CRON_TZ=", "TZ="} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			name, _, _ := strings.Cut(rest, " ")
			if name == "" || strings.EqualFold(name, "local") {
				return time.UTC
			}
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
			return time.UTC
		}
	}
	return time.UTC
}
