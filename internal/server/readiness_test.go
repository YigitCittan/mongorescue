package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestJobRPOValidationAndDefaults(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	h := srv.buildRoutes()
	ctx := context.Background()

	post := func(rpo int) (int, string) {
		body := `{"name":"n","database":"shop","cron_expression":"@hourly","connection_id":"` + testConnID + `","rpo_minutes":` + strconv.Itoa(rpo) + `}`
		rec := serve(h, "POST", "/api/v1/jobs", []byte(body), nil)
		return rec.Code, rec.Body.String()
	}
	for _, bad := range []int{-1, 1, 14, models.MaxRPOMinutes + 1} {
		if code, body := post(bad); code != http.StatusBadRequest || !strings.Contains(body, "rpo_minutes") {
			t.Errorf("POST rpo_minutes=%d = %d %s; want 400 naming rpo_minutes", bad, code, body)
		}
	}
	for _, ok := range []int{0, models.MinRPOMinutes, models.MaxRPOMinutes} {
		if code, body := post(ok); code != http.StatusCreated {
			t.Errorf("POST rpo_minutes=%d = %d %s; want 201", ok, code, body)
		}
	}

	created := createTestJob(t, h, models.Job{Name: "rpo", Database: "shop", CronExpression: "@every 6h", ConnectionID: testConnID, Enabled: true, RPOMinutes: 120})
	if created.RPOMinutes != 120 {
		t.Fatalf("created rpo_minutes = %d", created.RPOMinutes)
	}
	getDetails := func() (rpo, effective int, isDefault bool) {
		var d struct {
			RPOMinutes          int  `json:"rpo_minutes"`
			EffectiveRPOMinutes int  `json:"effective_rpo_minutes"`
			RPODefault          bool `json:"rpo_default"`
		}
		decodeData(t, serve(h, "GET", "/api/v1/jobs/"+created.ID, nil, nil), &d)
		return d.RPOMinutes, d.EffectiveRPOMinutes, d.RPODefault
	}
	if rpo, eff, def := getDetails(); rpo != 120 || eff != 120 || def {
		t.Fatalf("GET = %d %d %v; want 120 120 false", rpo, eff, def)
	}

	put := func(body string) int {
		return serve(h, "PUT", "/api/v1/jobs/"+created.ID, []byte(body), nil).Code
	}
	base := `"name":"rpo","database":"shop","cron_expression":"@every 6h","connection_id":"` + testConnID + `"`
	// Omitted keeps the objective; out of range is refused; 0 restores the default.
	if code := put(`{` + base + `}`); code != http.StatusOK {
		t.Fatalf("PUT without rpo_minutes = %d", code)
	}
	if rpo, _, _ := getDetails(); rpo != 120 {
		t.Fatalf("rpo_minutes after a PUT without it = %d; want 120", rpo)
	}
	if code := put(`{` + base + `,"rpo_minutes":10}`); code != http.StatusBadRequest {
		t.Fatalf("PUT rpo_minutes=10 = %d; want 400", code)
	}
	if code := put(`{` + base + `,"rpo_minutes":0}`); code != http.StatusOK {
		t.Fatalf("PUT rpo_minutes=0 = %d", code)
	}
	if rpo, eff, def := getDetails(); rpo != 0 || eff != 13*60 || !def {
		t.Fatalf("GET after the reset = %d %d %v; want 0 780 true", rpo, eff, def)
	}
	stored, err := metaStore.GetJob(ctx, created.ID)
	if err != nil || stored.RPOMinutes != 0 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	// The schedule preview reports the default objective of a schedule.
	var p struct {
		DefaultRPOMinutes int `json:"default_rpo_minutes"`
	}
	decodeData(t, serve(h, "GET", "/api/v1/schedule/preview?cron="+url.QueryEscape("@daily"), nil, nil), &p)
	if p.DefaultRPOMinutes != 49*60 {
		t.Fatalf("default_rpo_minutes(@daily) = %d; want %d", p.DefaultRPOMinutes, 49*60)
	}
}

func TestReadinessEndpoint(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	if got := requiredScope("GET /api/v1/readiness"); got != auth.ScopeRead {
		t.Fatalf("readiness scope = %s; want read", got)
	}
	now := time.Now().UTC()
	if err := f.store.SaveJob(ctx, &models.Job{ID: "job_a", Name: "a", Database: "shop", CronExpression: "@hourly",
		Enabled: true, ConnectionID: testConnID, CreatedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	done := now.Add(-time.Hour)
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", JobID: "job_a", Database: "shop", ConnectionID: testConnID,
		Status: models.StatusCompleted, StartedAt: done.Add(-time.Minute), CompletedAt: &done}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range auth.Scopes() {
		rec := serve(f.h, "GET", "/api/v1/readiness", nil, map[string]string{"X-API-Key": f.keys[scope]})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/readiness = %d %s", scope, rec.Code, rec.Body.String())
		}
	}
	rec := serve(f.h, "GET", "/api/v1/readiness", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("the readiness report leaks the connection's password")
	}
	var report struct {
		Rows []struct {
			ConnectionID   string   `json:"connection_id"`
			ConnectionName string   `json:"connection_name"`
			Database       string   `json:"database"`
			Status         string   `json:"status"`
			Reasons        []string `json:"reasons"`
			Jobs           []struct {
				ID            string  `json:"id"`
				TargetSeconds float64 `json:"target_seconds"`
				Met           bool    `json:"met"`
			} `json:"jobs"`
			RPO struct {
				Met *bool `json:"met"`
			} `json:"rpo"`
			LastGoodBackup *struct {
				ID string `json:"id"`
			} `json:"last_good_backup"`
		} `json:"rows"`
		Summary map[string]int `json:"summary"`
	}
	decodeData(t, rec, &report)
	if len(report.Rows) != 1 {
		t.Fatalf("rows = %+v", report.Rows)
	}
	r := report.Rows[0]
	if r.ConnectionID != testConnID || r.ConnectionName != "test server" || r.Database != "shop" || r.Status != "warn" ||
		r.LastGoodBackup == nil || r.LastGoodBackup.ID != "b1" || r.RPO.Met == nil || !*r.RPO.Met ||
		len(r.Jobs) != 1 || r.Jobs[0].TargetSeconds != (6*time.Hour).Seconds() || report.Summary["warn"] != 1 {
		t.Fatalf("row = %+v, summary %v", r, report.Summary)
	}
}
