package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestHistoryEmpty(t *testing.T) {
	svc := newServiceWith(t, storetest.New(t))
	h, err := svc.History(context.Background(), operations.HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Days != operations.DefaultHistoryDays || len(h.Daily) != operations.DefaultHistoryDays {
		t.Fatalf("days = %d, daily = %d; want %d", h.Days, len(h.Daily), operations.DefaultHistoryDays)
	}
	for _, d := range h.Daily {
		if d.Completed+d.Failed+d.Cancelled != 0 || d.StoredBytes != 0 {
			t.Fatalf("empty store reports %+v", d)
		}
	}
	if len(h.Jobs) != 0 || len(h.Upcoming) != 0 || len(h.VerificationIssues) != 0 || h.VerificationIssuesTotal != 0 || h.ServerTimeZone.Name == "" {
		t.Fatalf("empty history = %+v", h)
	}
}

func TestHistoryRejectsOutOfRange(t *testing.T) {
	svc := newServiceWith(t, storetest.New(t))
	for _, req := range []operations.HistoryRequest{
		{Days: -1}, {Days: operations.MaxHistoryDays + 1}, {OffsetMinutes: 15 * 60}, {OffsetMinutes: -15 * 60},
	} {
		if _, err := svc.History(context.Background(), req); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("History(%+v) = %v; want ErrInvalid", req, err)
		}
	}
}

func TestHistoryAggregatesAndUpcoming(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 10, 30, 0, 0, time.UTC)
	jobs := []*models.Job{
		{ID: "j_hourly", Name: "hourly", Database: "shop", CronExpression: "@hourly", Enabled: true},
		{ID: "j_off", Name: "off", Database: "shop", CronExpression: "@hourly", Enabled: false},
		{ID: "j_minute", Name: "minute", Database: "shop", CronExpression: "* * * * *", Enabled: true},
	}
	for _, j := range jobs {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	recs := []*models.BackupRecord{
		{ID: "old", JobID: "j_hourly", Database: "shop", Status: models.StatusCompleted, StartedAt: now.AddDate(0, 0, -40), SizeBytes: 500},
		{ID: "b1", JobID: "j_hourly", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-49 * time.Hour), SizeBytes: 100, DurationSeconds: 3},
		{ID: "b2", JobID: "j_hourly", Database: "shop", Status: models.StatusFailed, StartedAt: now.Add(-time.Hour)},
		{ID: "b3", JobID: "j_hourly", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-10 * time.Minute), SizeBytes: 20,
			Verification: models.VerificationMismatch},
	}
	for _, r := range recs {
		if err := st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	svc := newServiceWith(t, st)
	operations.SetNow(svc, func() time.Time { return now })

	h, err := svc.History(ctx, operations.HistoryRequest{Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Daily) != 7 || h.Daily[6].Date != "2026-09-30" || h.Daily[0].Date != "2026-09-24" || !h.From.Equal(time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = %s .. %s from %v", h.Daily[0].Date, h.Daily[len(h.Daily)-1].Date, h.From)
	}
	if h.StoredBytesBefore != 500 {
		t.Errorf("stored before = %d; want 500", h.StoredBytesBefore)
	}
	last, twoDaysAgo := h.Daily[6], h.Daily[4]
	if last.Completed != 1 || last.Failed != 1 || last.Bytes != 20 || last.StoredBytes != 620 {
		t.Errorf("today = %+v", last)
	}
	if twoDaysAgo.Completed != 1 || twoDaysAgo.StoredBytes != 600 || h.Daily[3].StoredBytes != 500 {
		t.Errorf("stored curve = %+v", h.Daily)
	}
	hj := h.Jobs["j_hourly"]
	if len(hj.Runs) != 4 || hj.Runs[3].ID != "b3" || hj.LastSuccessAt == nil || !hj.LastSuccessAt.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("job history = %+v", hj)
	}
	if h.VerificationIssuesTotal != 1 || h.VerificationIssues[0].ID != "b3" {
		t.Errorf("verification issues = %+v", h.VerificationIssues)
	}

	// 24 hourly runs, the minute job capped, the disabled job left out; in order.
	hourly, minute := 0, 0
	for i, u := range h.Upcoming {
		if i > 0 && u.At.Before(h.Upcoming[i-1].At) {
			t.Fatalf("upcoming runs out of order at %d", i)
		}
		if !u.At.After(now) || u.At.After(now.Add(operations.UpcomingWindow)) {
			t.Fatalf("upcoming run %v outside the window", u.At)
		}
		switch u.JobID {
		case "j_hourly":
			hourly++
		case "j_minute":
			minute++
		default:
			t.Fatalf("unexpected upcoming job %s", u.JobID)
		}
	}
	if hourly != 24 || minute != operations.MaxUpcomingPerJob || !h.UpcomingTruncated {
		t.Errorf("upcoming hourly=%d minute=%d truncated=%v", hourly, minute, h.UpcomingTruncated)
	}

	// A time zone three hours east: the window ends on the caller's today.
	east, err := svc.History(ctx, operations.HistoryRequest{Days: 1, OffsetMinutes: 180})
	if err != nil || len(east.Daily) != 1 || east.Daily[0].Date != "2026-09-30" || !east.From.Equal(time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("east = %+v, %v", east, err)
	}
}

func TestPreviewSchedule(t *testing.T) {
	svc := newServiceWith(t, storetest.New(t))
	now := time.Date(2026, 9, 30, 10, 30, 0, 0, time.Local)
	operations.SetNow(svc, func() time.Time { return now })

	p, err := svc.PreviewSchedule("0 2 * * *", 3)
	if err != nil || !p.Valid || len(p.NextRuns) != 3 {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if got := p.NextRuns[0].In(time.Local); got.Hour() != 2 || got.Minute() != 0 || !got.After(now) {
		t.Errorf("first run = %v", got)
	}

	bad, err := svc.PreviewSchedule("61 * * * *", 3)
	if err != nil || bad.Valid || bad.Error == "" || len(bad.NextRuns) != 0 {
		t.Fatalf("invalid preview = %+v, %v", bad, err)
	}
	for _, tc := range []struct {
		expr string
		n    int
	}{{"", 3}, {"@daily", 0}, {"@daily", operations.MaxSchedulePreviewRuns + 1}, {string(make([]byte, 300)), 1}} {
		if _, err := svc.PreviewSchedule(tc.expr, tc.n); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("PreviewSchedule(%q, %d) = %v; want ErrInvalid", tc.expr, tc.n, err)
		}
	}
}
