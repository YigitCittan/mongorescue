package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// dayStarts returns n+1 midnights in loc starting at y-m-d.
func dayStarts(loc *time.Location, y int, m time.Month, d, n int) []time.Time {
	out := make([]time.Time, n+1)
	for i := range out {
		out[i] = time.Date(y, m, d+i, 0, 0, 0, 0, loc)
	}
	return out
}

func TestBackupHistoryEmpty(t *testing.T) {
	q := store.BackupHistoryQuery{DayStarts: dayStarts(time.UTC, 2026, 9, 1, 30), RunsPerJob: 10, MaxIssues: 10}
	h, err := storetest.New(t).BackupHistory(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Days) != 0 || h.BytesBefore != 0 || len(h.JobRuns) != 0 || len(h.LastSuccess) != 0 || len(h.VerificationIssues) != 0 || h.VerificationIssueTotal != 0 {
		t.Fatalf("empty history = %+v", h)
	}
}

func TestBackupHistoryRejectsBadDays(t *testing.T) {
	s := storetest.New(t)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, starts := range [][]time.Time{nil, {now}, {now, now}, {now.Add(time.Hour), now}, dayStarts(time.UTC, 2025, 1, 1, store.MaxHistoryDays+1)} {
		if _, err := s.BackupHistory(context.Background(), store.BackupHistoryQuery{DayStarts: starts}); !errors.Is(err, store.ErrInvalidHistory) {
			t.Errorf("%d boundaries: %v; want ErrInvalidHistory", len(starts), err)
		}
	}
}

func TestBackupHistoryAggregates(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	for _, id := range []string{"j1", "j2", "j3"} {
		if err := s.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "shop", CronExpression: "@daily", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	day := func(d, hour int) time.Time { return time.Date(2026, 9, d, hour, 0, 0, 0, time.UTC) }
	recs := []*models.BackupRecord{
		// Before the window: only its size counts (BytesBefore); its failed
		// verification is outside the window.
		{ID: "old", JobID: "j1", Database: "shop", Status: models.StatusCompleted, StartedAt: day(1, 12), SizeBytes: 1000, DurationSeconds: 5,
			Verification: models.VerificationMismatch},
		{ID: "oldfail", JobID: "j1", Database: "shop", Status: models.StatusFailed, StartedAt: day(1, 13), SizeBytes: 77},
		{ID: "a", JobID: "j1", Database: "shop", Status: models.StatusCompleted, StartedAt: day(10, 1), SizeBytes: 100, DurationSeconds: 12.5,
			Verification: models.VerificationMismatch},
		{ID: "b", JobID: "j1", Database: "shop", Status: models.StatusFailed, StartedAt: day(10, 22), SizeBytes: 9},
		{ID: "c", JobID: "j1", Database: "shop", Status: models.StatusCompleted, StartedAt: day(11, 3), SizeBytes: 50, DurationSeconds: 7,
			Verification: models.VerificationOK},
		{ID: "d", JobID: "j2", Database: "crm", Status: models.StatusCancelled, StartedAt: day(11, 4)},
		{ID: "e", Database: "crm", Status: models.StatusCompleted, StartedAt: day(12, 5), SizeBytes: 25, Verification: models.VerificationError},
		{ID: "f", JobID: "j2", Database: "crm", Status: models.StatusInProgress, StartedAt: day(12, 6)},
		// A failed backup with a failed verification is not reported: it is not restorable anyway.
		{ID: "g", JobID: "j2", Database: "crm", Status: models.StatusFailed, StartedAt: day(12, 7), Verification: models.VerificationMismatch},
		// A deleted job's backups are not listed per job.
		{ID: "h", JobID: "j_gone", Database: "crm", Status: models.StatusCompleted, StartedAt: day(12, 8)},
	}
	for _, r := range recs {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	h, err := s.BackupHistory(ctx, store.BackupHistoryQuery{DayStarts: dayStarts(time.UTC, 2026, 9, 10, 3), RunsPerJob: 2, MaxIssues: 1})
	if err != nil {
		t.Fatal(err)
	}
	if h.BytesBefore != 1000 {
		t.Errorf("bytes before = %d; want 1000", h.BytesBefore)
	}
	want := []store.BackupDay{
		{Day: 0, Completed: 1, Failed: 1, CompletedBytes: 100},
		{Day: 1, Completed: 1, Cancelled: 1, CompletedBytes: 50},
		{Day: 2, Completed: 2, Failed: 1, CompletedBytes: 25},
	}
	if len(h.Days) != len(want) {
		t.Fatalf("days = %+v; want %+v", h.Days, want)
	}
	for i := range want {
		if h.Days[i] != want[i] {
			t.Errorf("day %d = %+v; want %+v", i, h.Days[i], want[i])
		}
	}

	// The two newest runs of each job, oldest first, with their durations.
	j1 := h.JobRuns["j1"]
	if len(j1) != 2 || j1[0].ID != "b" || j1[1].ID != "c" || j1[1].DurationSeconds != 7 || j1[1].Status != models.StatusCompleted ||
		!j1[1].StartedAt.Equal(day(11, 3)) {
		t.Errorf("j1 runs = %+v", j1)
	}
	if j2 := h.JobRuns["j2"]; len(j2) != 2 || j2[0].ID != "f" || j2[1].ID != "g" {
		t.Errorf("j2 runs = %+v", j2)
	}
	for _, id := range []string{"", "j_gone", "j3"} {
		if _, ok := h.JobRuns[id]; ok {
			t.Errorf("runs listed for %q", id)
		}
	}
	if got := h.LastSuccess["j1"]; !got.Equal(day(11, 3)) {
		t.Errorf("j1 last success = %v", got)
	}
	if _, ok := h.LastSuccess["j2"]; ok {
		t.Error("j2 has no completed backup but a last success")
	}
	if h.VerificationIssueTotal != 2 || len(h.VerificationIssues) != 1 || h.VerificationIssues[0].ID != "e" ||
		h.VerificationIssues[0].Verification != models.VerificationError || h.VerificationIssues[0].Database != "crm" {
		t.Errorf("verification issues = %d %+v", h.VerificationIssueTotal, h.VerificationIssues)
	}

	// Days three hours east of UTC: b (22:00 UTC) belongs to the next day.
	east := time.FixedZone("UTC+3", 3*3600)
	he, err := s.BackupHistory(ctx, store.BackupHistoryQuery{DayStarts: dayStarts(east, 2026, 9, 10, 3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(he.Days) != 3 || he.Days[0].Failed != 0 || he.Days[1].Failed != 1 || he.Days[1].Completed != 1 {
		t.Errorf("east days = %+v", he.Days)
	}
	if len(he.JobRuns) != 0 || len(he.VerificationIssues) != 0 || he.VerificationIssueTotal != 2 {
		t.Errorf("runs and issues were not asked for: %+v", he)
	}
}
