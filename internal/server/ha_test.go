package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSwitchingHandler(t *testing.T) {
	status := func() HAStatus {
		return HAStatus{Role: HARoleActive, Cluster: "prod", Epoch: 7, Holder: "a", LeaseHealthy: true, RetryAfterSeconds: 10}
	}
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("metrics")) })
	h := SwitchingHandler(status, "v1.2.3", metrics)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var health struct {
		Data struct {
			Status  string   `json:"status"`
			Version string   `json:"version"`
			Role    string   `json:"role"`
			HA      HAStatus `json:"ha"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || health.Data.Role != HARoleSwitching || health.Data.HA.Role != HARoleSwitching ||
		health.Data.HA.Cluster != "prod" || health.Data.HA.Epoch != 7 || health.Data.Version != "v1.2.3" {
		t.Fatalf("health while switching = %d %s; want 200 with the role switching", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "metrics" {
		t.Fatalf("metrics while switching = %d %q", rec.Code, rec.Body.String())
	}

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodPost, "/api/v1/health", nil),
		httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodGet, "/", nil),
	} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body apiResponse
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" ||
			json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Success || !strings.Contains(body.Error, "changing role") {
			t.Errorf("%s %s while switching = %d (Retry-After %q) %s; want a 503 JSON error with Retry-After: 1",
				req.Method, req.URL.Path, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
		}
	}

	// Without a metrics handler, /metrics is refused like the rest.
	rec = httptest.NewRecorder()
	SwitchingHandler(status, "", nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("metrics without a handler = %d; want 503", rec.Code)
	}
}
