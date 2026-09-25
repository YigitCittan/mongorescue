package scheduler

import (
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
