package scheduler

import (
	"errors"
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	from := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		expr string
		want *time.Time
	}{
		{name: "daily at 03:00", expr: "0 3 * * *", want: ptr(time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC))},
		{name: "hourly descriptor", expr: "@hourly", want: ptr(time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC))},
		{name: "invalid expression", expr: "not a cron", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextRun(tt.expr, from)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("nextRun(%q) = %v, want nil", tt.expr, *got)
			case tt.want != nil && (got == nil || !got.Equal(*tt.want)):
				t.Fatalf("nextRun(%q) = %v, want %v", tt.expr, got, *tt.want)
			}
		})
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestNextRunsAndValidateCron(t *testing.T) {
	from := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC)
	got := NextRuns("@hourly", from, 3)
	want := []time.Time{
		time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("NextRuns = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("NextRuns = %v, want %v", got, want)
		}
	}
	if NextRuns("not a cron", from, 3) != nil || NextRuns("@daily", from, 0) != nil {
		t.Fatal("NextRuns of an invalid expression or n <= 0 should be nil")
	}
	for _, ok := range []string{"@daily", "@every 6h", "0 3 * * 1-5"} {
		if err := ValidateCron(ok); err != nil {
			t.Errorf("ValidateCron(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "61 * * * *", "every tuesday", "0 0 * * * *"} {
		if err := ValidateCron(bad); !errors.Is(err, ErrInvalidCron) {
			t.Errorf("ValidateCron(%q) = %v; want ErrInvalidCron", bad, err)
		}
	}
}
