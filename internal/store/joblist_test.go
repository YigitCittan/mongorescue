package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// pageAll reads every job of q in windows of size, returning their IDs.
func pageAll(t *testing.T, s *store.SQLiteStore, q store.JobQuery, size int) []string {
	t.Helper()
	q.Limit = size
	var ids []string
	for range 1000 {
		w, err := s.QueryJobs(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range w.Jobs {
			ids = append(ids, j.ID)
		}
		if !w.More {
			return ids
		}
		q.AfterName, q.AfterID = w.LastName, w.LastID
	}
	t.Fatal("paging did not end")
	return nil
}

func TestQueryJobsPagesInNameOrder(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	if w, err := s.QueryJobs(ctx, store.JobQuery{Limit: 10}); err != nil || len(w.Jobs) != 0 || w.More {
		t.Fatalf("empty store: %+v, %v", w, err)
	}
	// Equal names are ordered by ID; names sort bytewise.
	names := map[string]string{"j1": "beta", "j2": "alpha", "j3": "beta", "j4": "Gamma", "j5": "alpha", "j6": "delta"}
	for id, name := range names {
		if err := s.SaveJob(ctx, &models.Job{ID: id, Name: name, Database: "db", Enabled: id != "j6", ConnectionID: map[bool]string{true: "c1", false: "c2"}[id < "j4"]}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"j4", "j2", "j5", "j1", "j3", "j6"}
	for _, size := range []int{1, 2, 3, 6, 7} {
		if got := pageAll(t, s, store.JobQuery{}, size); !slices.Equal(got, want) {
			t.Errorf("windows of %d = %v; want %v", size, got, want)
		}
	}
	// A window ending exactly on the last job reports no more.
	if w, _ := s.QueryJobs(ctx, store.JobQuery{Limit: 6}); w.More || len(w.Jobs) != 6 || w.LastID != "j6" {
		t.Errorf("exact window = %d jobs, more %v, last %s", len(w.Jobs), w.More, w.LastID)
	}
	if got := pageAll(t, s, store.JobQuery{ConnectionID: "c1"}, 2); !slices.Equal(got, []string{"j2", "j1", "j3"}) {
		t.Errorf("connection c1 = %v", got)
	}
	off := false
	if got := pageAll(t, s, store.JobQuery{Enabled: &off}, 2); !slices.Equal(got, []string{"j6"}) {
		t.Errorf("paused = %v", got)
	}
	if got := pageAll(t, s, store.JobQuery{Connections: auth.OnlyConnections("c2")}, 1); !slices.Equal(got, []string{"j4", "j5", "j6"}) {
		t.Errorf("access c2 = %v", got)
	}
	if got := pageAll(t, s, store.JobQuery{Connections: auth.OnlyConnections()}, 1); len(got) != 0 {
		t.Errorf("no access = %v", got)
	}
	for q, n := range map[*store.JobQuery]int{
		{}: 6, {ConnectionID: "c2"}: 3, {Enabled: &off}: 1, {Connections: auth.OnlyConnections("c1")}: 3,
	} {
		if got, err := s.CountJobs(ctx, *q); err != nil || got != n {
			t.Errorf("CountJobs(%+v) = %d, %v; want %d", *q, got, err, n)
		}
	}
	// A job deleted between two windows does not shift the next one.
	w, _ := s.QueryJobs(ctx, store.JobQuery{Limit: 2})
	if err := s.DeleteJob(ctx, "j5"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	next, _ := s.QueryJobs(ctx, store.JobQuery{Limit: 2, AfterName: w.LastName, AfterID: w.LastID})
	if ids := []string{next.Jobs[0].ID, next.Jobs[1].ID}; !slices.Equal(ids, []string{"j3", "j6"}) || next.More {
		t.Errorf("window after deletions = %v, more %v", ids, next.More)
	}
	for _, limit := range []int{-1, store.MaxJobWindow + 1} {
		if _, err := s.QueryJobs(ctx, store.JobQuery{Limit: limit}); !errors.Is(err, store.ErrInvalidFilter) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

func TestQueryJobsSkipsUnreadableRowsWithoutLosingTheCursor(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "db"}); err != nil {
			t.Fatal(err)
		}
	}
	storetest.CorruptRow(t, s, "jobs", "b", "[1]")
	w, err := s.QueryJobs(ctx, store.JobQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The unreadable row ends the window: the cursor is past it, and c follows.
	if len(w.Jobs) != 1 || w.LastID != "b" || !w.More {
		t.Fatalf("window = %d jobs, last %s, more %v", len(w.Jobs), w.LastID, w.More)
	}
	if got := pageAll(t, s, store.JobQuery{}, 1); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("readable jobs = %v", got)
	}
}

func TestBatchedJobQueriesMatchThePerJobOnes(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var ids []string
	statuses := []models.JobRunStatus{models.JobRunOK, models.JobRunSkipped, models.JobRunPartial, models.JobRunFailed}
	for i := range 7 {
		id := fmt.Sprintf("job_%d", i)
		ids = append(ids, id)
		for k := range i * 3 {
			run := &models.JobRun{ID: fmt.Sprintf("run_%d_%02d", i, k), JobID: id, Status: statuses[(i+k)%len(statuses)], StartedAt: now.Add(time.Duration(k) * time.Minute)}
			if err := s.SaveJobRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			b := &models.BackupRecord{ID: fmt.Sprintf("bkp_%d_%02d", i, k), JobID: id, Database: fmt.Sprintf("db%d", k%3),
				Status: []models.BackupStatus{models.StatusCompleted, models.StatusFailed}[k%2], StartedAt: now.Add(time.Duration(k) * time.Minute)}
			if err := s.SaveBackupRecord(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids = append(ids, "job_missing")
	runs, err := s.RecentExecutedJobRuns(ctx, ids, 4)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestJobsDatabaseBackups(ctx, ids, models.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	anyStatus, err := s.LatestJobsDatabaseBackups(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		one, err := s.ListExecutedJobRuns(ctx, id, 4)
		if err != nil {
			t.Fatal(err)
		}
		if got := runIDs(runs[id]); !slices.Equal(got, runIDs(one)) {
			t.Errorf("%s runs = %v; per job %v", id, got, runIDs(one))
		}
		for status, m := range map[models.BackupStatus]map[string]map[string]*models.BackupRecord{models.StatusCompleted: latest, "": anyStatus} {
			per, err := s.LatestJobDatabaseBackups(ctx, id, status)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for db, b := range m[id] {
				got[db] = b.ID
			}
			wantIDs := map[string]string{}
			for db, b := range per {
				wantIDs[db] = b.ID
			}
			if !reflect.DeepEqual(got, wantIDs) {
				t.Errorf("%s latest %q = %v; per job %v", id, status, got, wantIDs)
			}
		}
	}
}

func runIDs(list []*models.JobRun) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}
