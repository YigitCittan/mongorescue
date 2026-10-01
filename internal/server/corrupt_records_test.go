package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestStatsExposeCorruptRecordsToAdminsOnly(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	for _, id := range []string{"job_a", "job_bad"} {
		if err := f.store.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "shop", CronExpression: "@daily", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	const secretish = "do-not-echo-7731"
	storetest.CorruptRow(t, f.store, "jobs", "job_bad", `{"id":"job_bad","name":["`+secretish+`"]}`)

	// The job list keeps working with the readable job.
	rec := serve(f.h, "GET", "/api/v1/jobs", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	var jobs struct {
		Success bool          `json:"success"`
		Data    []*models.Job `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil || !jobs.Success || len(jobs.Data) != 1 || jobs.Data[0].ID != "job_a" {
		t.Fatalf("GET /api/v1/jobs = %d %s", rec.Code, rec.Body.String())
	}

	for _, scope := range auth.Scopes() {
		rec := serve(f.h, "GET", "/api/v1/stats", nil, map[string]string{"X-API-Key": f.keys[scope]})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/stats = %d %s", scope, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), secretish) {
			t.Fatalf("%s: stats echo stored data: %s", scope, rec.Body.String())
		}
		var body struct {
			Data struct {
				ActiveJobs     int `json:"active_jobs"`
				CorruptRecords []struct {
					Table string `json:"table"`
					ID    string `json:"id"`
					Error string `json:"error"`
				} `json:"corrupt_records"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Data.ActiveJobs != 1 {
			t.Errorf("%s: active jobs = %d; want 1", scope, body.Data.ActiveJobs)
		}
		got := body.Data.CorruptRecords
		if scope != auth.ScopeAdmin {
			if strings.Contains(rec.Body.String(), "corrupt_records") {
				t.Errorf("%s key sees corrupt records: %s", scope, rec.Body.String())
			}
			continue
		}
		if len(got) != 1 || got[0].Table != "jobs" || got[0].ID != "job_bad" || got[0].Error == "" {
			t.Errorf("admin corrupt records = %+v; want jobs/job_bad", got)
		}
	}
}
