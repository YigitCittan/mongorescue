package operations_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// sortEnv stores five jobs with ties in every order: two named alpha, two with the
// same next run, two created at the same time, two whose last run failed.
func sortEnv(t *testing.T) *bulkEnv {
	t.Helper()
	env := newBulkEnv(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	t0 := now.Add(-24 * time.Hour)
	for _, j := range []*models.Job{
		{ID: "j1", Name: "alpha", Enabled: true, NextRun: at(2 * time.Hour), CreatedAt: t0, LastRun: at(-time.Hour)},
		{ID: "j2", Name: "alpha", Enabled: true, NextRun: at(time.Hour), CreatedAt: t0, LastRun: at(-2 * time.Hour)},
		{ID: "j3", Name: "beta", NextRun: at(3 * time.Hour), CreatedAt: t0.Add(time.Minute), LastRun: at(-30 * time.Minute)},
		{ID: "j4", Name: "gamma", Enabled: true, NextRun: at(time.Hour), CreatedAt: t0.Add(-time.Minute)},
		{ID: "j5", Name: "delta", Enabled: true, CreatedAt: t0.Add(time.Minute), LastRun: at(-3 * time.Hour)},
	} {
		j.Database, j.CronExpression, j.ConnectionID = "shop", "0 3 * * *", "c1"
		if err := env.st.SaveJob(admin(), j); err != nil {
			t.Fatal(err)
		}
	}
	env.backupAt(t, "b1", "j1", models.StatusFailed, now.Add(-time.Hour), "", 0)
	env.backupAt(t, "b2", "j2", models.StatusCompleted, now.Add(-2*time.Hour), "", 1)
	env.backupAt(t, "b3", "j3", models.StatusCompleted, now.Add(-30*time.Minute), "", 1)
	env.backupAt(t, "b5", "j5", models.StatusFailed, now.Add(-3*time.Hour), "", 0)
	return env
}

func TestListJobsPageSortsBothWays(t *testing.T) {
	env := sortEnv(t)
	for _, tc := range []struct {
		sort      string
		asc, desc []string
	}{
		{"", []string{"j1", "j2", "j3", "j5", "j4"}, []string{"j4", "j5", "j3", "j2", "j1"}},
		{operations.JobSortName, []string{"j1", "j2", "j3", "j5", "j4"}, []string{"j4", "j5", "j3", "j2", "j1"}},
		// Paused jobs and jobs without a next run last in both directions.
		{operations.JobSortNextRun, []string{"j2", "j4", "j1", "j3", "j5"}, []string{"j1", "j4", "j2", "j5", "j3"}},
		{operations.JobSortCreatedAt, []string{"j4", "j1", "j2", "j3", "j5"}, []string{"j5", "j3", "j2", "j1", "j4"}},
		// Failures first, the newest run first within an outcome; never ran last.
		{operations.JobSortLastStatus, []string{"j1", "j5", "j3", "j2", "j4"}, []string{"j2", "j3", "j5", "j1", "j4"}},
	} {
		for _, desc := range []bool{false, true} {
			want := tc.asc
			if desc {
				want = tc.desc
			}
			// Indexed (no filter) and evaluated (a search matching every job) pages
			// agree, whatever the page size.
			for _, f := range []operations.BulkFilter{{}, {Q: "j"}} {
				for _, size := range []int{1, 2, 3, 5} {
					pages, total := allPages(t, env.svc, operations.JobListRequest{Sort: tc.sort, Desc: desc, Limit: size, Filter: f})
					var got []string
					for _, p := range pages {
						got = append(got, p...)
					}
					if total != 5 || !slices.Equal(got, want) {
						t.Errorf("sort %q desc=%v filter %+v size %d = %v; want %v", tc.sort, desc, f, size, got, want)
					}
				}
			}
		}
	}
}

// decodeCursor reads the JSON inside a jobs cursor.
func decodeCursor(t *testing.T, c string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(raw), "j1:") {
		t.Fatalf("cursor %q: %v", c, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw[3:], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListJobsPageCursorBelongsToItsOrder(t *testing.T) {
	env := sortEnv(t)
	p, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Sort: operations.JobSortNextRun, Desc: true, Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("first page: %+v, %v", p, err)
	}
	// The cursor names the order, the direction and the last job's key.
	c := decodeCursor(t, p.Next)
	key, _ := c["k"].(map[string]any)
	if c["s"] != "next_run" || c["d"] != true || key["id"] != "j1" || key["r"] == nil {
		t.Fatalf("cursor = %v", c)
	}
	for _, req := range []operations.JobListRequest{
		{After: p.Next},
		{Sort: operations.JobSortNextRun, After: p.Next},
		{Sort: operations.JobSortLastStatus, Desc: true, After: p.Next},
		{Sort: "size"},
	} {
		if _, err = env.svc.ListJobsPage(admin(), req); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("ListJobsPage(%+v) = %v; want ErrInvalid", req, err)
		}
	}
	// A cursor survives the deletion of its job and of the jobs around it.
	for _, id := range []string{"j1", "j4"} {
		if err = env.svc.DeleteJob(admin(), id); err != nil {
			t.Fatal(err)
		}
	}
	next, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Sort: operations.JobSortNextRun, Desc: true, Limit: 5, After: p.Next})
	if err != nil || !slices.Equal(pageIDs(next.Items), []string{"j2", "j5", "j3"}) {
		t.Fatalf("after the deleted j1 = %v, %v", pageIDs(next.Items), err)
	}
}
