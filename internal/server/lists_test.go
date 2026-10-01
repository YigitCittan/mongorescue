package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// listResponse is the decoded shape of a list endpoint.
type listResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Meta    *listMeta       `json:"meta"`
	Error   string          `json:"error"`
}

func getList(t *testing.T, h http.Handler, target string) (int, listResponse, []map[string]any) {
	t.Helper()
	rec := serve(h, "GET", target, nil, nil)
	var resp listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s: %v (%s)", target, err, rec.Body)
	}
	var items []map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(resp.Data, &items); err != nil {
			t.Fatalf("GET %s: data is not an array: %s", target, resp.Data)
		}
	}
	return rec.Code, resp, items
}

func itemIDs(items []map[string]any) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, fmt.Sprint(it["id"]))
	}
	return ids
}

// seedLists stores 30 backups (bkp_00 oldest, every third failed, bkp_05 retried by
// bkp_29) and 5 restores.
func seedLists(t *testing.T, st store.Store) time.Time {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range 30 {
		rec := &models.BackupRecord{
			ID: fmt.Sprintf("bkp_%02d", i), Database: []string{"shop", "crm"}[i%2], Status: models.StatusCompleted,
			Trigger: models.TriggerManual, StartedAt: t0.Add(time.Duration(i) * time.Hour),
		}
		if i%3 == 0 {
			rec.Status = models.StatusFailed
		}
		if i < 10 {
			rec.JobID, rec.Trigger = "job_a", models.TriggerScheduled
		}
		if i == 29 {
			rec.RetryOf = "bkp_05"
		}
		if err := st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if err := st.SaveRestoreRecord(ctx, &models.RestoreRecord{
			ID: fmt.Sprintf("rst_%02d", i), BackupID: "bkp_01", SourceDatabase: "crm",
			TargetDatabase: []string{"crm_rescue", "crm"}[i%2], Status: models.RestoreStatusCompleted,
			StartedAt: t0.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return t0
}

func TestListBackupsWithoutLimitKeepsTheShape(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	seedLists(t, st)
	h := srv.buildRoutes()

	code, resp, items := getList(t, h, "/api/v1/backups")
	if code != http.StatusOK || !resp.Success || resp.Meta != nil || len(items) != 30 {
		t.Fatalf("GET /api/v1/backups = %d, meta %+v, %d items; want 200, no meta, 30 items", code, resp.Meta, len(items))
	}
	if items[0]["id"] != "bkp_29" || items[29]["id"] != "bkp_00" {
		t.Fatalf("order = %v; want newest first", itemIDs(items))
	}
	// Filters without limit still return every match, with no meta.
	code, resp, items = getList(t, h, "/api/v1/backups?job_id=job_a&status=failed")
	if code != http.StatusOK || resp.Meta != nil || strings.Join(itemIDs(items), ",") != "bkp_09,bkp_06,bkp_03,bkp_00" {
		t.Fatalf("job_id+status = %d %v meta %+v", code, itemIDs(items), resp.Meta)
	}
	code, _, items = getList(t, h, "/api/v1/restores")
	if code != http.StatusOK || len(items) != 5 {
		t.Fatalf("GET /api/v1/restores = %d, %d items", code, len(items))
	}
}

func TestListBackupsPagedCarriesMeta(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	t0 := seedLists(t, st)
	h := srv.buildRoutes()

	code, resp, items := getList(t, h, "/api/v1/backups?limit=25")
	if code != http.StatusOK || resp.Meta == nil || *resp.Meta != (listMeta{Total: 30, Limit: 25, Offset: 0}) || len(items) != 25 {
		t.Fatalf("limit=25 = %d meta %+v, %d items", code, resp.Meta, len(items))
	}
	_, resp, items = getList(t, h, "/api/v1/backups?limit=25&offset=25")
	if *resp.Meta != (listMeta{Total: 30, Limit: 25, Offset: 25}) || strings.Join(itemIDs(items), ",") != "bkp_04,bkp_03,bkp_02,bkp_01,bkp_00" {
		t.Fatalf("second page = meta %+v, %v", resp.Meta, itemIDs(items))
	}
	_, resp, items = getList(t, h, "/api/v1/backups?limit=10&offset=100")
	if resp.Meta.Total != 30 || len(items) != 0 {
		t.Fatalf("past the end = meta %+v, %d items; want total 30 and []", resp.Meta, len(items))
	}

	q := url.Values{
		"database": {"crm"}, "trigger": {"manual"}, "sort": {"asc"}, "limit": {"3"},
		"from": {t0.Add(10 * time.Hour).Format(time.RFC3339)}, "to": {t0.Add(20 * time.Hour).Format(time.RFC3339)},
	}
	_, resp, items = getList(t, h, "/api/v1/backups?"+q.Encode())
	if resp.Meta.Total != 5 || strings.Join(itemIDs(items), ",") != "bkp_11,bkp_13,bkp_15" {
		t.Fatalf("combined filter = meta %+v, %v; want bkp_11,13,15 of 5", resp.Meta, itemIDs(items))
	}
	_, resp, items = getList(t, h, "/api/v1/backups?q=BKP_2&limit=200")
	if resp.Meta.Total != 10 || len(items) != 10 {
		t.Fatalf("search = meta %+v, %d items", resp.Meta, len(items))
	}

	// Retry links work across pages: the retry is on page 1, the original on page 2.
	_, _, items = getList(t, h, "/api/v1/backups?retry_of=bkp_05&limit=5")
	if strings.Join(itemIDs(items), ",") != "bkp_29" || items[0]["retry_of"] != "bkp_05" {
		t.Fatalf("retry_of = %v", items)
	}
	_, _, items = getList(t, h, "/api/v1/backups?limit=5&offset=24")
	for _, it := range items {
		rb, has := it["retried_by"].(map[string]any)
		if (it["id"] == "bkp_05") != has || (has && rb["id"] != "bkp_29") {
			t.Fatalf("%v retried_by = %v", it["id"], it["retried_by"])
		}
	}

	_, resp, items = getList(t, h, "/api/v1/restores?database=crm&backup_id=bkp_01&status=completed&limit=1&sort=asc")
	if *resp.Meta != (listMeta{Total: 2, Limit: 1, Offset: 0}) || strings.Join(itemIDs(items), ",") != "rst_01" {
		t.Fatalf("restores = meta %+v, %v", resp.Meta, itemIDs(items))
	}
}

func TestListParamsAreValidated(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	seedLists(t, st)
	h := srv.buildRoutes()
	for _, tc := range []struct{ target, want string }{
		{"/api/v1/backups?limit=0", "limit"},
		{"/api/v1/backups?limit=201", "limit"},
		{"/api/v1/backups?limit=ten", "limit"},
		{"/api/v1/backups?limit=-5", "limit"},
		{"/api/v1/backups?offset=-1", "offset"},
		{"/api/v1/backups?offset=x", "offset"},
		{"/api/v1/backups?sort=random", "sort"},
		{"/api/v1/backups?from=yesterday", "from"},
		{"/api/v1/backups?to=2026-13-01", "to"},
		{"/api/v1/backups?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z", "to must not be before from"},
		{"/api/v1/backups?status=exploded", "status"},
		{"/api/v1/backups?status=completed'%20OR%20'1'='1", "status"},
		{"/api/v1/backups?trigger=cron", "trigger"},
		{"/api/v1/backups?q=" + strings.Repeat("x", 300), "q"},
		{"/api/v1/restores?limit=999", "limit"},
		{"/api/v1/restores?status=pruned", "status"},
		{"/api/v1/restores?from=1", "from"},
	} {
		rec := serve(h, "GET", tc.target, nil, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("GET %s = %d %s; want 400 mentioning %q", tc.target, rec.Code, rec.Body, tc.want)
		}
	}
	// Injection-looking values in free filters are just values that match nothing.
	code, _, items := getList(t, h, "/api/v1/backups?database="+url.QueryEscape("shop' OR '1'='1")+"&q="+url.QueryEscape("%"))
	if code != http.StatusOK || len(items) != 0 {
		t.Fatalf("hostile filters = %d, %d items; want 200 and none", code, len(items))
	}
}

func TestListDatabasesAndStats(t *testing.T) {
	srv, st, _ := setupTestServer(t)
	seedLists(t, st)
	h := srv.buildRoutes()

	for target, want := range map[string]string{
		"/api/v1/backups/databases":  `["crm","shop"]`,
		"/api/v1/restores/databases": `["crm","crm_rescue"]`,
	} {
		rec := serve(h, "GET", target, nil, nil)
		var resp struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK || string(resp.Data) != want {
			t.Fatalf("GET %s = %d %s; want %s", target, rec.Code, rec.Body, want)
		}
	}

	rec := serve(h, "GET", "/api/v1/stats", nil, nil)
	var stats struct {
		Data struct {
			TotalBackups   int                  `json:"total_backups"`
			FailedBackups  int                  `json:"failed_backups"`
			TotalRestores  int                  `json:"total_restores"`
			LastBackup     *struct{ ID string } `json:"last_backup"`
			JobLastBackups map[string]struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"job_last_backups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	d := stats.Data
	if d.TotalBackups != 30 || d.FailedBackups != 10 || d.TotalRestores != 5 || d.LastBackup == nil || d.LastBackup.ID != "bkp_29" {
		t.Fatalf("stats = %+v", d)
	}
	if last := d.JobLastBackups["job_a"]; last.ID != "bkp_09" || last.Status != "failed" {
		t.Fatalf("job_a last backup = %+v; want bkp_09 failed", last)
	}
}
