package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestStatsHistoryEndpoint(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	if err := f.store.SaveJob(ctx, &models.Job{ID: "job_a", Name: "a", Database: "shop", CronExpression: "@hourly", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, r := range []*models.BackupRecord{
		{ID: "b1", JobID: "job_a", Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-time.Minute), SizeBytes: 64, DurationSeconds: 2},
		{ID: "b2", JobID: "job_a", Database: "shop", Status: models.StatusFailed, StartedAt: now.Add(-2 * time.Minute)},
	} {
		if err := f.store.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// Every scope may read it (read is the lowest).
	for _, scope := range auth.Scopes() {
		rec := serve(f.h, "GET", "/api/v1/stats/history?days=7&tz_offset=0", nil, map[string]string{"X-API-Key": f.keys[scope]})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/stats/history = %d %s", scope, rec.Code, rec.Body.String())
		}
	}
	if got := requiredScope("GET /api/v1/stats/history"); got != auth.ScopeRead {
		t.Fatalf("history scope = %s; want read", got)
	}

	rec := serve(f.h, "GET", "/api/v1/stats/history?days=7", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	var h struct {
		Days  int `json:"days"`
		Daily []struct {
			Date        string `json:"date"`
			Completed   int    `json:"completed"`
			Failed      int    `json:"failed"`
			StoredBytes int64  `json:"stored_bytes"`
		} `json:"daily"`
		Jobs map[string]struct {
			Runs []struct {
				ID              string  `json:"id"`
				DurationSeconds float64 `json:"duration_seconds"`
			} `json:"runs"`
			LastSuccessAt *time.Time `json:"last_success_at"`
		} `json:"jobs"`
		Upcoming []struct {
			JobID string    `json:"job_id"`
			At    time.Time `json:"at"`
		} `json:"upcoming"`
		ServerTimeZone struct {
			Name string `json:"name"`
		} `json:"server_time_zone"`
	}
	decodeData(t, rec, &h)
	if h.Days != 7 || len(h.Daily) != 7 || h.ServerTimeZone.Name == "" {
		t.Fatalf("history = %+v", h)
	}
	completed, failed := 0, 0
	for _, d := range h.Daily {
		completed += d.Completed
		failed += d.Failed
	}
	if completed != 1 || failed != 1 || h.Daily[6].StoredBytes != 64 {
		t.Errorf("daily = %+v", h.Daily)
	}
	if j := h.Jobs["job_a"]; len(j.Runs) != 2 || j.Runs[1].ID != "b1" || j.Runs[1].DurationSeconds != 2 || j.LastSuccessAt == nil {
		t.Errorf("job history = %+v", j)
	}
	if len(h.Upcoming) < 23 || h.Upcoming[0].JobID != "job_a" {
		t.Errorf("upcoming = %d runs", len(h.Upcoming))
	}

	for q, want := range map[string]string{"tz=Asia%2FTokyo&tz_offset=0": "Asia/Tokyo", "tz=Nowhere%2FCity&tz_offset=-90": "UTC-01:30"} {
		var zone struct {
			TimeZone string `json:"time_zone"`
		}
		decodeData(t, serve(f.h, "GET", "/api/v1/stats/history?"+q, nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]}), &zone)
		if zone.TimeZone != want {
			t.Errorf("?%s: time_zone = %q; want %q", q, zone.TimeZone, want)
		}
	}
	for _, q := range []string{"days=0", "days=400", "days=x", "tz_offset=9999", "tz_offset=x"} {
		rec := serve(f.h, "GET", "/api/v1/stats/history?"+q, nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d %s; want 400", q, rec.Code, rec.Body.String())
		}
	}
}

func TestStatsHistoryEmptyStore(t *testing.T) {
	f := newScopeFixture(t)
	rec := serve(f.h, "GET", "/api/v1/stats/history", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	var h struct {
		Days               int              `json:"days"`
		Daily              []map[string]any `json:"daily"`
		Jobs               map[string]any   `json:"jobs"`
		Upcoming           []map[string]any `json:"upcoming"`
		VerificationIssues []map[string]any `json:"verification_issues"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/stats/history = %d %s", rec.Code, rec.Body.String())
	}
	decodeData(t, rec, &h)
	if h.Days != 30 || len(h.Daily) != 30 || h.Jobs == nil || h.Upcoming == nil || h.VerificationIssues == nil {
		t.Fatalf("empty history = %+v", h)
	}
}

func TestSchedulePreviewEndpoint(t *testing.T) {
	f := newScopeFixture(t)
	if got := requiredScope("GET /api/v1/schedule/preview"); got != auth.ScopeRead {
		t.Fatalf("preview scope = %s; want read", got)
	}
	get := func(q string) (int, map[string]any) {
		rec := serve(f.h, "GET", "/api/v1/schedule/preview?"+q, nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var out map[string]any
		decodeData(t, rec, &out)
		return rec.Code, out
	}
	code, p := get("cron=" + url.QueryEscape("0 2 * * *"))
	if code != http.StatusOK || p["valid"] != true || len(p["next_runs"].([]any)) != 3 {
		t.Fatalf("preview = %d %v", code, p)
	}
	code, p = get("cron=" + url.QueryEscape("@every 6h") + "&n=5")
	if code != http.StatusOK || len(p["next_runs"].([]any)) != 5 {
		t.Fatalf("preview n=5 = %d %v", code, p)
	}
	code, p = get("cron=" + url.QueryEscape("99 * * * *"))
	if code != http.StatusOK || p["valid"] != false || p["error"] == "" {
		t.Fatalf("invalid preview = %d %v", code, p)
	}
	for _, q := range []string{"", "cron=%40daily&n=0", "cron=%40daily&n=x", "cron=%40daily&n=11"} {
		if code, _ := get(q); code != http.StatusBadRequest {
			t.Errorf("?%s = %d; want 400", q, code)
		}
	}
}
