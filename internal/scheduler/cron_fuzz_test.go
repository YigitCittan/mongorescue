package scheduler

import (
	"errors"
	"testing"
	"time"
)

// FuzzValidateCron checks that ValidateCron and NextRuns never panic or hang, that
// they agree on which expressions are valid, and that the runs NextRuns returns are
// strictly increasing and after the start time.
func FuzzValidateCron(f *testing.F) {
	for _, s := range []string{
		"0 2 * * *", "*/5 * * * *", "0 0 1 1 *", "@daily", "@hourly", "@every 6h", "@every 1s", "@every 0s",
		"@every -1h", "@yearly", "0 0 30 2 *", "0 0 31 4 *", "59 23 31 12 *", "0-59/7 0-23 1-31 1-12 0-6",
		"0 0 * * MON-FRI", "0 0 * JAN,JUL SUN", "", " ", "* * * *", "* * * * * *", "60 * * * *", "0 24 * * *",
		"*/0 * * * *", "1-0 * * * *", "?", "@reboot", "TZ=UTC 0 0 * * *", "CRON_TZ=Europe/Istanbul @daily",
		"TZ=", "TZ=UTC", "CRON_TZ=", "TZ=../../etc/passwd 0 0 * * *", "0 0 ? * *", "0 0 L * *",
		"0 0 1-31/100000000000000000000 * *", "@every 9223372036854775807ns",
	} {
		f.Add(s)
	}
	from := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, expr string) {
		start := time.Now()
		err := ValidateCron(expr)
		runs := NextRuns(expr, from, 5)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ValidateCron/NextRuns(%q) took %v", expr, elapsed)
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidCron) {
				t.Fatalf("ValidateCron(%q) = %v; want ErrInvalidCron", expr, err)
			}
			if runs != nil {
				t.Fatalf("NextRuns(%q) = %v for an invalid expression", expr, runs)
			}
			return
		}
		prev := from
		for _, r := range runs {
			if !r.After(prev) || r.Location() != time.UTC {
				t.Fatalf("NextRuns(%q) = %v is not increasing UTC after %v", expr, runs, from)
			}
			prev = r
		}
	})
}
