package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/heartbeat"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// fakeLiveness is a scheduler liveness source with a fixed last tick.
type fakeLiveness struct{ last time.Time }

func (f fakeLiveness) LastTick() time.Time { return f.last }
func (f fakeLiveness) Stale(now time.Time) bool {
	return !f.last.IsZero() && now.Sub(f.last) > scheduler.StaleAfter
}

func getHealth(t *testing.T, srv *Server) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.buildRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var res struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return rec.Code, res.Data
}

func TestHealthReportsTheScheduler(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	if code, data := getHealth(t, srv); code != http.StatusOK || data["scheduler"] != schedulerNotStarted || data["status"] != "healthy" {
		t.Fatalf("not started: %d %v", code, data)
	}

	srv.livenessSource = fakeLiveness{last: time.Now().Add(-10 * time.Second)}
	code, data := getHealth(t, srv)
	if code != http.StatusOK || data["scheduler"] != schedulerOK || data["status"] != "healthy" || data["scheduler_last_tick"] == nil {
		t.Fatalf("fresh tick: %d %v", code, data)
	}
	if data["version"] == nil || data["time"] == nil {
		t.Fatalf("the existing fields must stay: %v", data)
	}

	// A hung scheduler: its last tick is older than three intervals.
	srv.livenessSource = fakeLiveness{last: time.Now().Add(-scheduler.StaleAfter - time.Minute)}
	code, data = getHealth(t, srv)
	if code != http.StatusServiceUnavailable || data["scheduler"] != schedulerStale || data["status"] != "unhealthy" {
		t.Fatalf("stale tick: %d %v", code, data)
	}
}

func TestHealthWithAStartedScheduler(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	if err := srv.scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, data := getHealth(t, srv); code != http.StatusOK || data["scheduler"] != schedulerOK {
		t.Fatalf("started scheduler: %d %v", code, data)
	}
	srv.scheduler.Stop()
}

func serveJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	raw := rec.Body.String()
	var res struct {
		Data any `json:"data"`
	}
	_ = json.Unmarshal([]byte(raw), &res)
	m, _ := res.Data.(map[string]any)
	return rec.Code, m, raw
}

func TestJobHeartbeatURLIsMaskedAndKept(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	mux := srv.buildRoutes()
	const secretURL = "https://hc-ping.com/job-secret-uuid"
	const masked = "https://hc-ping.com/" + models.SecretMask
	job := map[string]any{
		"name": "nightly", "database": "analytics", "cron_expression": "@daily", "enabled": true,
		"connection_id": testConnID, "heartbeat_url": secretURL,
	}
	code, created, raw := serveJSON(t, mux, http.MethodPost, "/api/v1/jobs", job)
	if code != http.StatusCreated || created["heartbeat_url"] != masked || strings.Contains(raw, "job-secret-uuid") {
		t.Fatalf("create: %d %s", code, raw)
	}
	id := created["id"].(string)
	stored, err := metaStore.GetJob(context.Background(), id)
	if err != nil || stored.HeartbeatURL != secretURL {
		t.Fatalf("stored job = %+v, %v", stored, err)
	}
	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/" + id} {
		if code, _, raw = serveJSON(t, mux, http.MethodGet, path, nil); code != http.StatusOK || strings.Contains(raw, "job-secret-uuid") || !strings.Contains(raw, "hc-ping.com") {
			t.Fatalf("GET %s: %d %s", path, code, raw)
		}
	}

	// Sending the masked value back keeps the URL (PUT and POST).
	update := map[string]any{"name": "nightly", "database": "analytics", "cron_expression": "@daily",
		"connection_id": testConnID, "heartbeat_url": masked}
	if code, _, raw = serveJSON(t, mux, http.MethodPut, "/api/v1/jobs/"+id, update); code != http.StatusOK || strings.Contains(raw, "job-secret-uuid") {
		t.Fatalf("PUT masked: %d %s", code, raw)
	}
	job["id"], job["heartbeat_url"] = id, masked
	if code, _, raw = serveJSON(t, mux, http.MethodPost, "/api/v1/jobs", job); code != http.StatusCreated {
		t.Fatalf("POST masked: %d %s", code, raw)
	}
	if stored, _ = metaStore.GetJob(context.Background(), id); stored.HeartbeatURL != secretURL {
		t.Fatalf("heartbeat after masked saves = %q", stored.HeartbeatURL)
	}

	// A masked value for another host is refused; invalid URLs too.
	for _, bad := range []string{"https://evil.example.com/" + models.SecretMask, "ftp://hc-ping.com/x"} {
		update["heartbeat_url"] = bad
		if code, _, raw = serveJSON(t, mux, http.MethodPut, "/api/v1/jobs/"+id, update); code != http.StatusBadRequest {
			t.Fatalf("PUT %q: %d %s", bad, code, raw)
		}
	}
	// "" removes the heartbeat.
	update["heartbeat_url"] = ""
	if code, _, raw = serveJSON(t, mux, http.MethodPut, "/api/v1/jobs/"+id, update); code != http.StatusOK {
		t.Fatalf("PUT empty: %d %s", code, raw)
	}
	if stored, _ = metaStore.GetJob(context.Background(), id); stored.HeartbeatURL != "" {
		t.Fatalf("heartbeat after clearing = %q", stored.HeartbeatURL)
	}
	// A new job cannot start from a masked value.
	delete(job, "id")
	job["heartbeat_url"] = masked
	if code, _, raw = serveJSON(t, mux, http.MethodPost, "/api/v1/jobs", job); code != http.StatusBadRequest {
		t.Fatalf("POST new masked: %d %s", code, raw)
	}
}

func TestHeartbeatTestEndpoint(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	mux := srv.buildRoutes()
	if code, _, _ := serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("without a heartbeat service: %d", code)
	}

	var mu sync.Mutex
	var paths []string
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/down") {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(monitor.Close)
	srv.heartbeat = heartbeat.New(heartbeat.Config{Client: monitor.Client(), Logger: slog.New(slog.DiscardHandler)})
	srv.settings = newTestSettings(t, metaStore.(settings.Repository), settings.Defaults().Security)
	mux = srv.buildRoutes()

	if code, _, raw := serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("nothing configured: %d %s", code, raw)
	}
	stored := monitor.URL + "/global-secret"
	if _, err := srv.settings.Update(context.Background(), settings.Patch{Monitoring: &settings.MonitoringPatch{HeartbeatURL: &stored}}); err != nil {
		t.Fatal(err)
	}
	// The stored URL (empty body, or its masked form).
	for _, body := range []any{nil, map[string]any{"heartbeat_url": models.MaskEndpoint(stored)}} {
		if code, data, raw := serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", body); code != http.StatusOK || data["ok"] != true || strings.Contains(raw, "global-secret") {
			t.Fatalf("test stored: %d %s", code, raw)
		}
	}
	// A URL being entered, and a failing one (host only in the error).
	if code, _, raw := serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", map[string]any{"heartbeat_url": monitor.URL + "/typed"}); code != http.StatusOK {
		t.Fatalf("test typed: %d %s", code, raw)
	}
	code, _, raw := serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", map[string]any{"heartbeat_url": monitor.URL + "/secret-path/down"})
	if code != http.StatusBadGateway || strings.Contains(raw, "secret-path") {
		t.Fatalf("test failing: %d %s", code, raw)
	}
	// A masked URL for another host is refused without a ping.
	if code, _, raw = serveJSON(t, mux, http.MethodPost, "/api/v1/settings/monitoring/test", map[string]any{"heartbeat_url": "https://evil.example.com/" + models.SecretMask}); code != http.StatusBadRequest {
		t.Fatalf("test masked other host: %d %s", code, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/global-secret", "/global-secret", "/typed", "/secret-path/down"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("pings = %v; want %v", paths, want)
	}
}

func TestSettingsMaskTheHeartbeatURL(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	srv.settings = newTestSettings(t, metaStore.(settings.Repository), settings.Defaults().Security)
	mux := srv.buildRoutes()
	patch := map[string]any{"monitoring": map[string]any{"heartbeat_url": "https://hc-ping.com/settings-secret", "heartbeat_interval": "10m"}}
	code, _, raw := serveJSON(t, mux, http.MethodPut, "/api/v1/settings", patch)
	if code != http.StatusOK || strings.Contains(raw, "settings-secret") || !strings.Contains(raw, `"heartbeat_interval":"10m0s"`) {
		t.Fatalf("PUT settings: %d %s", code, raw)
	}
	if code, _, raw = serveJSON(t, mux, http.MethodGet, "/api/v1/settings", nil); code != http.StatusOK || strings.Contains(raw, "settings-secret") || !strings.Contains(raw, "https://hc-ping.com/"+models.SecretMask) {
		t.Fatalf("GET settings: %d %s", code, raw)
	}
}
