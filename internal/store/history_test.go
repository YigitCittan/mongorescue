package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestBackupHistoryEmpty(t *testing.T) {
	h, err := storetest.New(t).BackupHistory(context.Background(), store.BackupHistoryQuery{From: time.Unix(0, 0), RunsPerJob: 10, MaxIssues: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Days) != 0 || h.BytesBefore != 0 || len(h.JobRuns) != 0 || len(h.LastSuccess) != 0 || len(h.VerificationIssues) != 0 || h.VerificationIssueTotal != 0 {
		t.Fatalf("empty history = %+v", h)
	}
}

func TestBackupHistoryAggregates(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	day := func(d, hour int) time.Time { return time.Date(2026, 9, d, hour, 0, 0, 0, time.UTC) }
	recs := []*models.BackupRecord{
		// Before the window: only its size counts (BytesBefore).
		{ID: "old", JobID: "j1", Database: "shop", Status: models.StatusCompleted, StartedAt: day(1, 12), SizeBytes: 1000, DurationSeconds: 5},
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
	}
	for _, r := range recs {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	h, err := s.BackupHistory(ctx, store.BackupHistoryQuery{From: day(10, 0), RunsPerJob: 2, MaxIssues: 1})
	if err != nil {
		t.Fatal(err)
	}
	if h.BytesBefore != 1000 {
		t.Errorf("bytes before = %d; want 1000", h.BytesBefore)
	}
	base := day(10, 0).Unix() / 86400
	want := []store.BackupDay{
		{Day: base, Completed: 1, Failed: 1, CompletedBytes: 100},
		{Day: base + 1, Completed: 1, Cancelled: 1, CompletedBytes: 50},
		{Day: base + 2, Completed: 1, Failed: 1, CompletedBytes: 25},
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
	if _, ok := h.JobRuns[""]; ok {
		t.Error("backups without a job are listed as a job")
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

	// A time zone three hours east moves b (22:00 UTC) into the next day.
	east, err := s.BackupHistory(ctx, store.BackupHistoryQuery{From: day(10, 0).Add(-3 * time.Hour), OffsetSeconds: 3 * 3600})
	if err != nil {
		t.Fatal(err)
	}
	if len(east.Days) != 3 || east.Days[0].Failed != 0 || east.Days[1].Failed != 1 || east.Days[1].Completed != 1 {
		t.Errorf("east days = %+v", east.Days)
	}
	if len(east.JobRuns) != 0 || len(east.VerificationIssues) != 0 || east.VerificationIssueTotal != 2 {
		t.Errorf("runs and issues were not asked for: %+v", east)
	}
}
