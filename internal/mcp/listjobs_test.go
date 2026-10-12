package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// list_jobs pages by key on the server, with the REST API's cursor.
func TestListJobsPagesByCursor(t *testing.T) {
	f := newFixture(t, nil)
	for i := range 4 {
		if err := f.store.SaveJob(context.Background(), &models.Job{ID: fmt.Sprintf("job_page_%d", i), Name: fmt.Sprintf("zz page %d", i),
			Database: "shop", CronExpression: "0 3 * * *", ConnectionID: testConnID}); err != nil {
			t.Fatal(err)
		}
	}
	read := f.session(t, principal(auth.ScopeRead))
	var ids []string
	cursor := ""
	for range 10 {
		args := map[string]any{"limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page jobList
		structured(t, ToolListJobs, call(t, read, ToolListJobs, args), &page)
		if page.Total != 5 {
			t.Fatalf("total = %d; want 5", page.Total)
		}
		for _, j := range page.Jobs {
			ids = append(ids, j.ID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	want := []string{testJobID, "job_page_0", "job_page_1", "job_page_2", "job_page_3"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("paged jobs = %v; want %v", ids, want)
	}
	for _, args := range []map[string]any{{"cursor": "nonsense"}, {"limit": 101}} {
		if res := call(t, read, ToolListJobs, args); !res.IsError {
			t.Errorf("list_jobs %v succeeded", args)
		}
	}
}
