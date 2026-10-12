package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// jobsPage is the decoded shape of GET /api/v1/jobs.
type jobsPage struct {
	Success bool          `json:"success"`
	Data    []*models.Job `json:"data"`
	Meta    *cursorMeta   `json:"meta"`
	Error   string        `json:"error"`
}

func getJobsPage(t *testing.T, h http.Handler, target string) (int, jobsPage) {
	t.Helper()
	rec := serve(h, "GET", target, nil, nil)
	var p jobsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("GET %s: %v (%s)", target, err, rec.Body)
	}
	return rec.Code, p
}

func TestListJobsPagesWithCursor(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()
	for i := range 230 {
		if err := st.SaveJob(ctx, &models.Job{ID: fmt.Sprintf("job_%03d", i), Name: fmt.Sprintf("job %03d", i), Database: "db",
			CronExpression: "0 3 * * *", Enabled: i%2 == 0, CreatedAt: time.Date(2026, 9, 1, 0, 0, i, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.buildRoutes()

	// Without a limit: the first 100 jobs, by name, and the next cursor.
	code, p := getJobsPage(t, h, "/api/v1/jobs")
	if code != http.StatusOK || !p.Success || len(p.Data) != 100 || p.Meta == nil || p.Meta.Total != 230 || p.Meta.Limit != 100 || p.Meta.Next == "" {
		t.Fatalf("default page: %d, %d jobs, meta %+v", code, len(p.Data), p.Meta)
	}
	if p.Data[0].ID != "job_000" || p.Data[99].ID != "job_099" {
		t.Fatalf("default page runs %s..%s", p.Data[0].ID, p.Data[99].ID)
	}
	var ids []string
	next := ""
	for range 10 {
		target := "/api/v1/jobs?limit=100"
		if next != "" {
			target += "&after=" + url.QueryEscape(next)
		}
		code, p = getJobsPage(t, h, target)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, code, p.Error)
		}
		for _, j := range p.Data {
			ids = append(ids, j.ID)
		}
		if next = p.Meta.Next; next == "" {
			break
		}
	}
	if len(ids) != 230 || !slices.IsSorted(ids) || ids[229] != "job_229" {
		t.Fatalf("paged %d jobs", len(ids))
	}
	// Filters and the cursor combine; total counts the matches.
	code, p = getJobsPage(t, h, "/api/v1/jobs?enabled=false&limit=500")
	if code != http.StatusOK || len(p.Data) != 115 || p.Meta.Total != 115 || p.Meta.Next != "" {
		t.Fatalf("paused jobs: %d, %d jobs, meta %+v", code, len(p.Data), p.Meta)
	}
	code, p = getJobsPage(t, h, "/api/v1/jobs?q=job+22&limit=5")
	if code != http.StatusOK || len(p.Data) != 5 || p.Meta.Total != 10 || p.Meta.Next == "" {
		t.Fatalf("search: %d, %d jobs, meta %+v", code, len(p.Data), p.Meta)
	}
	code, p = getJobsPage(t, h, "/api/v1/jobs?q=job+22&limit=5&after="+url.QueryEscape(p.Meta.Next))
	if code != http.StatusOK || len(p.Data) != 5 || p.Data[0].ID != "job_225" || p.Meta.Next != "" {
		t.Fatalf("search, second page: %d, %v, meta %+v", code, p.Data, p.Meta)
	}

	// Server-side orders: descending by creation time pages from the newest job.
	code, p = getJobsPage(t, h, "/api/v1/jobs?sort=created_at&order=desc&limit=2")
	if code != http.StatusOK || len(p.Data) != 2 || p.Data[0].ID != "job_229" || p.Data[1].ID != "job_228" || p.Meta.Next == "" {
		t.Fatalf("created_at desc: %d, %+v", code, p.Data)
	}
	code, p = getJobsPage(t, h, "/api/v1/jobs?sort=created_at&order=desc&limit=2&after="+url.QueryEscape(p.Meta.Next))
	if code != http.StatusOK || len(p.Data) != 2 || p.Data[0].ID != "job_227" {
		t.Fatalf("created_at desc, second page: %d, %+v", code, p.Data)
	}
	// A cursor of one order is refused by another.
	if code, _ := getJobsPage(t, h, "/api/v1/jobs?sort=name&after="+url.QueryEscape(p.Meta.Next)); code != http.StatusBadRequest {
		t.Errorf("a created_at cursor with sort=name = %d; want 400", code)
	}
	if code, _ := getJobsPage(t, h, "/api/v1/jobs?sort=created_at&order=asc&after="+url.QueryEscape(p.Meta.Next)); code != http.StatusBadRequest {
		t.Errorf("a descending cursor with order=asc = %d; want 400", code)
	}
	for _, target := range []string{"/api/v1/jobs?limit=0", "/api/v1/jobs?limit=501", "/api/v1/jobs?limit=x",
		"/api/v1/jobs?after=nonsense", "/api/v1/jobs?enabled=maybe", "/api/v1/jobs?schedule=yearly",
		"/api/v1/jobs?sort=size", "/api/v1/jobs?order=up", "/api/v1/jobs?sort=-name"} {
		if code, p := getJobsPage(t, h, target); code != http.StatusBadRequest || p.Success {
			t.Errorf("GET %s = %d; want 400", target, code)
		}
	}
}
