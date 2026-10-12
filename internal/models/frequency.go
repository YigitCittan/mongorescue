package models

import (
	"strings"
	"time"
)

// Schedule frequencies of a cron expression (see ScheduleFrequency): the values of
// the jobs list's schedule filter.
const (
	// FrequencyHourly is a schedule that runs more than once a day.
	FrequencyHourly = "hourly"
	// FrequencyDaily is a schedule that runs once every day.
	FrequencyDaily = "daily"
	// FrequencyWeekly is a schedule that runs on some days of the week.
	FrequencyWeekly = "weekly"
	// FrequencyMonthly is a schedule that runs on some days of the month.
	FrequencyMonthly = "monthly"
	// FrequencyOther is any other schedule (yearly, some months, unusual intervals).
	FrequencyOther = "other"
)

// ScheduleFrequency classifies a cron expression (five fields or a descriptor such as
// "@daily" or "@every 6h", with an optional time zone prefix) as FrequencyHourly,
// FrequencyDaily, FrequencyWeekly, FrequencyMonthly or FrequencyOther. Expressions it
// cannot read are FrequencyOther.
func ScheduleFrequency(expr string) string {
	s := strings.TrimSpace(expr)
	if strings.HasPrefix(s, "TZ=") || strings.HasPrefix(s, "CRON_TZ=") {
		_, rest, ok := strings.Cut(s, " ")
		if !ok {
			return FrequencyOther
		}
		s = strings.TrimSpace(rest)
	}
	switch s {
	case "@hourly":
		return FrequencyHourly
	case "@daily", "@midnight":
		return FrequencyDaily
	case "@weekly":
		return FrequencyWeekly
	case "@monthly":
		return FrequencyMonthly
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		return everyFrequency(strings.TrimSpace(rest))
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return FrequencyOther
	}
	minute, hour, dom, month, dow := f[0], f[1], f[2], f[3], f[4]
	every := func(v string) bool { return v == "*" || v == "?" }
	switch {
	case !every(month):
		return FrequencyOther
	case !singleCronValue(minute) || !singleCronValue(hour):
		return FrequencyHourly
	case every(dom) && every(dow):
		return FrequencyDaily
	case every(dom):
		return FrequencyWeekly
	case every(dow):
		return FrequencyMonthly
	}
	return FrequencyOther
}

// everyFrequency classifies the interval of an "@every" descriptor.
func everyFrequency(interval string) string {
	d, err := time.ParseDuration(interval)
	switch {
	case err != nil || d <= 0:
		return FrequencyOther
	case d < 24*time.Hour:
		return FrequencyHourly
	case d == 24*time.Hour:
		return FrequencyDaily
	case d == 7*24*time.Hour:
		return FrequencyWeekly
	}
	return FrequencyOther
}

// singleCronValue reports whether a cron field is one number (no list, range or
// step).
func singleCronValue(v string) bool {
	if v == "" || len(v) > 2 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
