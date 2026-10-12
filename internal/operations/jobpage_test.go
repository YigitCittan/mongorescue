package operations_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// pageIDs returns the IDs of jobs in order.
func pageIDs(jobs []*models.Job) []string {
	out := []string{}
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}

// allPages pages through ListJobsPage with req, returning the IDs of every page.
func allPages(t *testing.T, svc *operations.Service, req operations.JobListRequest) ([][]string, int) {
	t.Helper()
	var pages [][]string
	total := -1
	for range 100 {
		p, err := svc.ListJobsPage(admin(), req)
		if err != nil {
			t.Fatal(err)
		}
		if total >= 0 && p.Total != total {
			t.Fatalf("total changed between pages: %d, %d", total, p.Total)
		}
		total = p.Total
		pages = append(pages, pageIDs(p.Items))
		if p.Next == "" {
			return pages, total
		}
		req.After = p.Next
	}
	t.Fatal("paging did not end")
	return nil, 0
}

func TestListJobsPageEmpty(t *testing.T) {
	env := newBulkEnv(t)
	p, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 0 || p.Items == nil || p.Total != 0 || p.Next != "" || p.Limit != operations.DefaultJobPageSize {
		t.Fatalf("empty page = %+v", p)
	}
	p, err = env.svc.ListJobsPage(admin(), operations.JobListRequest{Filter: operations.BulkFilter{Q: "x"}})
	if err != nil || len(p.Items) != 0 || p.Total != 0 || p.Next != "" {
		t.Fatalf("empty filtered page = %+v, %v", p, err)
	}
}

func TestListJobsPagePagesByName(t *testing.T) {
	env := jobListEnv(t)
	// Names: Archive, New, Nightly CRM, Orders hourly, Weekly shop.
	want := []string{"j_monthly", "j_never", "j_daily", "j_hourly", "j_weekly"}
	pages, total := allPages(t, env.svc, operations.JobListRequest{Limit: 2})
	if total != 5 || !slices.Equal(pages[0], want[:2]) || !slices.Equal(pages[1], want[2:4]) || !slices.Equal(pages[2], want[4:]) || len(pages) != 3 {
		t.Fatalf("pages of 2 = %v (total %d)", pages, total)
	}
	// A page ending exactly on the last job has no next cursor.
	for _, limit := range []int{5, 0} {
		p, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: limit})
		if err != nil || !slices.Equal(pageIDs(p.Items), want) || p.Next != "" {
			t.Fatalf("limit %d = %v, next %q, %v", limit, pageIDs(p.Items), p.Next, err)
		}
	}
	p, _ := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 4})
	if p.Next == "" || len(p.Items) != 4 {
		t.Fatalf("limit 4 = %v, next %q", pageIDs(p.Items), p.Next)
	}
	last, _ := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 1, After: p.Next})
	if !slices.Equal(pageIDs(last.Items), []string{"j_weekly"}) || last.Next != "" {
		t.Fatalf("after the 4th job = %v, next %q", pageIDs(last.Items), last.Next)
	}
}

func TestListJobsPageDeletionBetweenPages(t *testing.T) {
	for _, f := range []operations.BulkFilter{{}, {Q: "j_"}} { // indexed and evaluated filters
		env := jobListEnv(t)
		first, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 2, Filter: f})
		if err != nil || !slices.Equal(pageIDs(first.Items), []string{"j_monthly", "j_never"}) {
			t.Fatalf("%+v: first page = %v, %v", f, pageIDs(first.Items), err)
		}
		// One job of the page shown and the first of the next one go meanwhile.
		for _, id := range []string{"j_never", "j_daily"} {
			if err = env.svc.DeleteJob(admin(), id); err != nil {
				t.Fatal(err)
			}
		}
		next, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 2, After: first.Next, Filter: f})
		if err != nil || !slices.Equal(pageIDs(next.Items), []string{"j_hourly", "j_weekly"}) || next.Next != "" || next.Total != 3 {
			t.Fatalf("%+v: next page = %v (next %q, total %d), %v", f, pageIDs(next.Items), next.Next, next.Total, err)
		}
	}
}

func TestListJobsPageFilterAndCursor(t *testing.T) {
	env := jobListEnv(t)
	on := true
	for _, tc := range []struct {
		name  string
		f     operations.BulkFilter
		pages [][]string
	}{
		{"enabled (indexed)", operations.BulkFilter{Enabled: &on}, [][]string{{"j_monthly", "j_daily"}, {"j_hourly"}}},
		{"connection (indexed)", operations.BulkFilter{ConnectionID: "c1"}, [][]string{{"j_never", "j_hourly"}, {"j_weekly"}}},
		{"database", operations.BulkFilter{Database: "crm"}, [][]string{{"j_never", "j_daily"}}},
		{"search", operations.BulkFilter{Q: "shop"}, [][]string{{"j_hourly", "j_weekly"}}},
		{"connection and schedule", operations.BulkFilter{ConnectionID: "c1", Schedule: operations.FrequencyDaily}, [][]string{{"j_never"}}},
		{"last status", operations.BulkFilter{LastStatus: operations.LastRunCompleted}, [][]string{{"j_monthly", "j_daily"}}},
		{"no match", operations.BulkFilter{Q: "nothing"}, [][]string{{}}},
	} {
		pages, total := allPages(t, env.svc, operations.JobListRequest{Limit: 2, Filter: tc.f})
		n := 0
		for _, p := range tc.pages {
			n += len(p)
		}
		if !slices.EqualFunc(pages, tc.pages, slices.Equal) || total != n {
			t.Errorf("%s = %v (total %d); want %v", tc.name, pages, total, tc.pages)
		}
	}
	// A cursor of one filter applies to another one by key.
	p, _ := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 1})
	q, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: 5, After: p.Next, Filter: operations.BulkFilter{ConnectionID: "c2"}})
	if err != nil || !slices.Equal(pageIDs(q.Items), []string{"j_daily"}) || q.Total != 2 {
		t.Fatalf("c2 after Archive = %v (total %d), %v", pageIDs(q.Items), q.Total, err)
	}
}

func TestListJobsPageValidation(t *testing.T) {
	env := jobListEnv(t)
	for _, req := range []operations.JobListRequest{
		{Limit: -1},
		{Limit: operations.MaxJobPageSize + 1},
		{After: "not a cursor"},
		{After: base64.RawURLEncoding.EncodeToString([]byte("o:5"))},
		{After: base64.RawURLEncoding.EncodeToString([]byte(`j1:{"n":"x"}`))},
		{Filter: operations.BulkFilter{Status: "failed"}},
		{Filter: operations.BulkFilter{Schedule: "yearly"}},
	} {
		if _, err := env.svc.ListJobsPage(admin(), req); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("ListJobsPage(%+v) = %v; want ErrInvalid", req, err)
		}
	}
	if p, err := env.svc.ListJobsPage(admin(), operations.JobListRequest{Limit: operations.MaxJobPageSize}); err != nil || len(p.Items) != 5 {
		t.Errorf("the largest limit: %v, %v", p, err)
	}
}

func TestListJobsPageFollowsConnectionAccess(t *testing.T) {
	env := jobListEnv(t)
	ctx := limitedTo(auth.ScopeRead, "c2")
	for _, f := range []operations.BulkFilter{{}, {Q: "j_"}} {
		p, err := env.svc.ListJobsPage(ctx, operations.JobListRequest{Limit: 1, Filter: f})
		if err != nil || !slices.Equal(pageIDs(p.Items), []string{"j_monthly"}) || p.Total != 2 || p.Next == "" {
			t.Fatalf("%+v: limited page = %v (total %d), %v", f, pageIDs(p.Items), p.Total, err)
		}
		p, err = env.svc.ListJobsPage(ctx, operations.JobListRequest{Limit: 1, After: p.Next, Filter: f})
		if err != nil || !slices.Equal(pageIDs(p.Items), []string{"j_daily"}) || p.Next != "" {
			t.Fatalf("%+v: limited second page = %v, %v", f, pageIDs(p.Items), err)
		}
	}
}
