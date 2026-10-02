package store

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// LatestJobRuns answers, in one query, what ListJobRuns(ctx, id, 1) answers for each
// job: the newest run by start time then ID, nothing for a job without runs or whose
// newest run cannot be read.
func TestLatestJobRunsMatchesListJobRuns(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	runs := []*models.JobRun{
		{ID: "r_a1", JobID: "job_a", Status: models.JobRunOK, StartedAt: t0},
		{ID: "r_a2", JobID: "job_a", Status: models.JobRunPartial, StartedAt: t0.Add(time.Hour)},
		// A tie on the start time: the higher ID is the newest, as in ListJobRuns.
		{ID: "r_b1", JobID: "job_b", Status: models.JobRunFailed, StartedAt: t0},
		{ID: "r_b2", JobID: "job_b", Status: models.JobRunCancelled, StartedAt: t0},
		{ID: "r_c1", JobID: "job_c", Status: models.JobRunOK, StartedAt: t0},
		{ID: "r_c2", JobID: "job_c", Status: models.JobRunRunning, StartedAt: t0.Add(time.Minute)},
		{ID: "r_other", JobID: "job_not_asked", Status: models.JobRunOK, StartedAt: t0},
	}
	for _, r := range runs {
		if err = s.SaveJobRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// job_c's newest run is unreadable (valid JSON of the wrong shape).
	if _, err = s.db.ExecContext(ctx, `UPDATE job_runs SET data = '{"id":"r_c2","databases":3}' WHERE id = 'r_c2'`); err != nil {
		t.Fatal(err)
	}
	ids := []string{"job_a", "job_b", "job_c", "job_none"}
	// Many IDs cross the query's chunk size.
	for i := range 2*latestJobRunsChunk + 3 {
		ids = append(ids, fmt.Sprintf("job_pad_%d", i))
	}
	got, err := s.LatestJobRuns(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		list, err := s.ListJobRuns(ctx, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if len(list) == 1 {
			want = list[0].ID
		}
		have := ""
		if r := got[id]; r != nil {
			have = r.ID
		}
		if have != want {
			t.Errorf("%s: LatestJobRuns = %q, ListJobRuns(1) = %q", id, have, want)
		}
	}
	if len(got) != 2 || got["job_a"].ID != "r_a2" || got["job_b"].ID != "r_b2" {
		t.Errorf("LatestJobRuns = %v; want job_a r_a2 and job_b r_b2 only", got)
	}
	if empty, err := s.LatestJobRuns(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("LatestJobRuns(nil) = %v, %v", empty, err)
	}
}
