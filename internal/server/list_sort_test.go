package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestListSortColumns(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, b := range []struct {
		id, db string
		size   int64
		secs   float64
		status models.BackupStatus
	}{
		{"b_a", "shop", 500, 3, models.StatusCompleted},
		{"b_b", "crm", 50, 90, models.StatusFailed},
		{"b_c", "analytics", 5000, 1, models.StatusCompleted},
	} {
		if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: b.id, Database: b.db, SizeBytes: b.size, DurationSeconds: b.secs,
			Status: b.status, StartedAt: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range []struct{ id, target string }{{"r_a", "zz"}, {"r_b", "aa"}} {
		if err := st.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: r.id, BackupID: "b_a", TargetDatabase: r.target,
			Status: models.RestoreStatusCompleted, StartedAt: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.buildRoutes()
	for target, want := range map[string]string{
		"/api/v1/backups":                             "b_c,b_b,b_a",
		"/api/v1/backups?sort=desc":                   "b_c,b_b,b_a",
		"/api/v1/backups?sort=asc":                    "b_a,b_b,b_c",
		"/api/v1/backups?sort=started_at":             "b_a,b_b,b_c",
		"/api/v1/backups?sort=-started_at":            "b_c,b_b,b_a",
		"/api/v1/backups?sort=-size":                  "b_c,b_a,b_b",
		"/api/v1/backups?sort=size&limit=2":           "b_b,b_a",
		"/api/v1/backups?sort=-duration":              "b_b,b_a,b_c",
		"/api/v1/backups?sort=database":               "b_c,b_b,b_a",
		"/api/v1/backups?sort=status":                 "b_c,b_a,b_b",
		"/api/v1/backups?sort=-size&status=completed": "b_c,b_a",
		"/api/v1/restores?sort=database":              "r_b,r_a",
		"/api/v1/restores?sort=-database":             "r_a,r_b",
	} {
		code, _, items := getList(t, h, target)
		if got := strings.Join(itemIDs(items), ","); code != http.StatusOK || got != want {
			t.Errorf("GET %s = %d %s; want 200 %s", target, code, got, want)
		}
	}
}

func TestListSortRejectsUnknownColumns(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	seedLists(t, st)
	h := srv.buildRoutes()
	for _, target := range []string{
		"/api/v1/backups?sort=id",
		"/api/v1/backups?sort=-data",
		"/api/v1/backups?sort=SIZE",
		"/api/v1/backups?sort=--size",
		"/api/v1/backups?sort=" + url.QueryEscape("size;DROP TABLE backups"),
		"/api/v1/backups?sort=" + url.QueryEscape("b.size_bytes"),
		"/api/v1/backups?sort=" + url.QueryEscape("started_at desc"),
		"/api/v1/restores?sort=size",
		"/api/v1/restores?sort=-size",
	} {
		rec := serve(h, "GET", target, nil, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sort must be") {
			t.Errorf("GET %s = %d %s; want 400 about sort", target, rec.Code, rec.Body)
		}
	}
	// The whitelist is named in the message; restores do not offer size.
	rec := serve(h, "GET", "/api/v1/restores?sort=size", nil, nil)
	if body := rec.Body.String(); !strings.Contains(body, "duration") || strings.Contains(body, "size,") {
		t.Errorf("restore sort message = %s", body)
	}
	// Data survived the hostile values.
	if code, _, items := getList(t, h, "/api/v1/backups"); code != http.StatusOK || len(items) != 30 {
		t.Fatalf("backups after hostile sorts = %d, %d items", code, len(items))
	}
}

func TestListJobsFilters(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	ctx := context.Background()
	for _, j := range []*models.Job{
		{ID: "job_hourly", Name: "Orders", Database: "shop", CronExpression: "0 * * * *", ConnectionID: "conn_a", Enabled: true},
		{ID: "job_daily", Name: "Nightly", Database: "crm", CronExpression: "0 2 * * *", ConnectionID: "conn_b", Enabled: true},
		{ID: "job_paused", Name: "Paused shop", Database: "shop", CronExpression: "@weekly", ConnectionID: "conn_a"},
	} {
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bk1", JobID: "job_daily", Database: "crm",
		Status: models.StatusFailed, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	h := srv.buildRoutes()
	ids := func(target string) (int, string) {
		t.Helper()
		rec := serve(h, "GET", target, nil, nil)
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		out := make([]string, 0, len(resp.Data))
		for _, j := range resp.Data {
			out = append(out, j.ID)
		}
		slices.Sort(out)
		return rec.Code, strings.Join(out, ",")
	}
	for target, want := range map[string]string{
		"/api/v1/jobs":                                  "job_daily,job_hourly,job_paused",
		"/api/v1/jobs?enabled=false":                    "job_paused",
		"/api/v1/jobs?enabled=true&database=shop":       "job_hourly",
		"/api/v1/jobs?connection_id=conn_a":             "job_hourly,job_paused",
		"/api/v1/jobs?schedule=hourly":                  "job_hourly",
		"/api/v1/jobs?schedule=weekly":                  "job_paused",
		"/api/v1/jobs?last_status=failed":               "job_daily",
		"/api/v1/jobs?last_status=never":                "job_hourly,job_paused",
		"/api/v1/jobs?q=SHOP":                           "job_hourly,job_paused",
		"/api/v1/jobs?q=" + url.QueryEscape("' OR 1=1"): "",
	} {
		if code, got := ids(target); code != http.StatusOK || got != want {
			t.Errorf("GET %s = %d %s; want 200 %s", target, code, got, want)
		}
	}
	for target, want := range map[string]string{
		"/api/v1/jobs?enabled=yes":                          "enabled",
		"/api/v1/jobs?schedule=yearly":                      "schedule",
		"/api/v1/jobs?last_status=ok":                       "last_status",
		"/api/v1/jobs?database=" + strings.Repeat("d", 300): "database",
	} {
		rec := serve(h, "GET", target, nil, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d %s; want 400 mentioning %q", target, rec.Code, rec.Body, want)
		}
	}
}
