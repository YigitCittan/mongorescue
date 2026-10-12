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
		last := w.Last
		q.After = &last
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
	if w, _ := s.QueryJobs(ctx, store.JobQuery{Limit: 6}); w.More || len(w.Jobs) != 6 || w.Last.ID != "j6" {
		t.Errorf("exact window = %d jobs, more %v, last %s", len(w.Jobs), w.More, w.Last.ID)
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
	next, _ := s.QueryJobs(ctx, store.JobQuery{Limit: 2, After: &w.Last})
	if ids := []string{next.Jobs[0].ID, next.Jobs[1].ID}; !slices.Equal(ids, []string{"j3", "j6"}) || next.More {
		t.Errorf("window after deletions = %v, more %v", ids, next.More)
	}
	for _, limit := range []int{-1, store.MaxJobWindow + 1} {
		if _, err := s.QueryJobs(ctx, store.JobQuery{Limit: limit}); !errors.Is(err, store.ErrInvalidFilter) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

// Every order pages stably in both directions, ties broken by ID, jobs without a
// next run last, and the keys the store computes match JobKeyOf.
func TestQueryJobsOrdersWithTies(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	ist := time.FixedZone("UTC+3", 3*3600)
	next := t0.Add(time.Hour).In(ist) // the same instant as job b's, written with an offset
	jobs := []*models.Job{
		{ID: "a", Name: "same", Enabled: true, NextRun: at(time.Hour), CreatedAt: t0},
		{ID: "b", Name: "same", Enabled: true, NextRun: &next, CreatedAt: t0},
		{ID: "c", Name: "alpha", Enabled: true, NextRun: at(-time.Hour), CreatedAt: t0.Add(time.Minute)},
		{ID: "d", Name: "zeta", Enabled: false, NextRun: at(-2 * time.Hour), CreatedAt: t0.Add(-time.Minute)},
		{ID: "e", Name: "same", Enabled: true, CreatedAt: t0},
		{ID: "f", Name: "Beta", Enabled: true, NextRun: at(90*time.Minute + 500*time.Millisecond), CreatedAt: t0.Add(time.Minute)},
		{ID: "g", Name: "mid", Enabled: false, CreatedAt: t0.Add(time.Hour)},
	}
	for _, j := range jobs {
		j.Database = "db"
		if err := s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	// SaveJob sets CreatedAt only when zero; reread the jobs as stored.
	stored, err := s.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sort := range []store.JobSort{store.JobSortName, store.JobSortCreatedAt, store.JobSortNextRun} {
		for _, desc := range []bool{false, true} {
			want := slices.Clone(stored)
			slices.SortFunc(want, func(x, y *models.Job) int {
				return store.CompareJobKeys(store.JobKeyOf(x, sort), store.JobKeyOf(y, sort), desc)
			})
			wantIDs := make([]string, len(want))
			for i, j := range want {
				wantIDs[i] = j.ID
			}
			for _, size := range []int{1, 2, 3, len(jobs)} {
				got := pageAll(t, s, store.JobQuery{Sort: sort, Desc: desc}, size)
				if !slices.Equal(got, wantIDs) {
					t.Errorf("%s desc=%v, windows of %d = %v; want %v", sort, desc, size, got, wantIDs)
				}
			}
			// The cursor of each one-job window is the job's key.
			q := store.JobQuery{Sort: sort, Desc: desc, Limit: 1}
			for _, j := range want {
				w, err := s.QueryJobs(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if k := store.JobKeyOf(j, sort); w.Last != k {
					t.Errorf("%s desc=%v: cursor of %s = %+v; JobKeyOf %+v", sort, desc, j.ID, w.Last, k)
				}
				last := w.Last
				q.After = &last
			}
		}
	}
	// Names: bytewise, ties by ID. Next run: the same instant in another zone ties.
	if got := pageAll(t, s, store.JobQuery{}, 2); !slices.Equal(got, []string{"f", "c", "g", "a", "b", "e", "d"}) {
		t.Errorf("by name = %v", got)
	}
	if got := pageAll(t, s, store.JobQuery{Sort: store.JobSortNextRun, Desc: true}, 2); !slices.Equal(got, []string{"f", "b", "a", "c", "g", "e", "d"}) {
		t.Errorf("by next run, descending = %v", got)
	}
	if _, err := s.QueryJobs(ctx, store.JobQuery{Sort: "size"}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Errorf("unknown order: %v", err)
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
	if len(w.Jobs) != 1 || w.Last.ID != "b" || !w.More {
		t.Fatalf("window = %d jobs, last %s, more %v", len(w.Jobs), w.Last.ID, w.More)
	}
	if got := pageAll(t, s, store.JobQuery{}, 1); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("readable jobs = %v", got)
	}
	// The count leaves the unreadable row out, as the pages do.
	if n, err := s.CountJobs(ctx, store.JobQuery{}); err != nil || n != 2 {
		t.Errorf("CountJobs = %d, %v; want 2", n, err)
	}
	if n, err := s.CountJobs(ctx, store.JobQuery{Enabled: new(bool)}); err != nil || n != 2 {
		t.Errorf("CountJobs = %d, %v; want 2", n, err)
	}
	// Repaired, it counts again.
	if err := s.SaveJob(ctx, &models.Job{ID: "b", Name: "b", Database: "db"}); err != nil {
		t.Fatal(err)
	}
	if got := pageAll(t, s, store.JobQuery{}, 5); len(got) != 3 {
		t.Fatalf("after the repair = %v", got)
	}
	if n, err := s.CountJobs(ctx, store.JobQuery{}); err != nil || n != 3 {
		t.Errorf("CountJobs after the repair = %d, %v; want 3", n, err)
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
