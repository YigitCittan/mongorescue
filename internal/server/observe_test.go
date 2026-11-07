package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/logging"
	"github.com/yigitcittan/mongorescue/internal/metrics"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

type observed struct {
	route, method string
	code          int
}

// observeServer returns the chain handler of a server with the request observer,
// the access log and a JSON logger, plus what they recorded.
func observeServer(t *testing.T, trusted []string, accessLog bool, withDashboard bool) (http.Handler, *[]observed, *bytes.Buffer) {
	t.Helper()
	srv, metaStore, _ := setupTestServer(t)
	cfg := newTestConfig()
	cfg.Security.TrustedProxies = trusted
	var (
		mu  sync.Mutex
		got []observed
		buf bytes.Buffer
	)
	logger := logging.New(&lockedWriter{mu: &mu, w: &buf}, logging.FormatJSON, slog.LevelInfo)
	var fsys fs.FS
	if withDashboard {
		fsys = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	}
	opts := []Option{
		WithConnections(srv.connections), WithAuth(newTestAuth(t, metaStore.(auth.Repository), testAPIKey)),
		WithSettings(newTestSettings(t, metaStore.(settings.Repository), cfg.Security)),
		WithAccessLog(accessLog),
		WithRequestObserver(func(route, method string, code int, _ time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, observed{route, method, code})
		}),
	}
	full := NewServer(bootConfig(), metaStore, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, fsys, logger, opts...)
	return full.Handler(), &got, &buf
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	h, _, _ := observeServer(t, nil, false, false)
	rec := serve(h, "GET", "/api/v1/health", nil, nil)
	id := rec.Header().Get(RequestIDHeader)
	if !validRequestID(id) || len(id) != 32 {
		t.Fatalf("generated request id = %q", id)
	}
	if other := serve(h, "GET", "/api/v1/health", nil, nil).Header().Get(RequestIDHeader); other == id {
		t.Error("two requests got the same id")
	}
}

func TestRequestIDFromTrustedProxyOnly(t *testing.T) {
	h, _, _ := observeServer(t, []string{"10.0.0.0/8"}, false, false)
	const want = "proxy-req-12345"
	send := func(remote, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/nope", nil)
		req.RemoteAddr = remote
		req.Header.Set(RequestIDHeader, id)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := send("10.1.2.3:5555", want)
	if got := rec.Header().Get(RequestIDHeader); got != want {
		t.Errorf("trusted proxy id = %q, want %q", got, want)
	}
	if got := send("203.0.113.9:5555", want).Header().Get(RequestIDHeader); got == want {
		t.Error("an untrusted peer chose the request id")
	}
	for _, bad := range []string{"short", strings.Repeat("a", 129), "has space in it", "inject\nline", "semi;colon-id"} {
		if got := send("10.1.2.3:5555", bad).Header().Get(RequestIDHeader); got == bad {
			t.Errorf("malformed id %q accepted", bad)
		}
	}
}

func TestAccessLogCarriesRouteNotPathOrQuery(t *testing.T) {
	h, _, buf := observeServer(t, nil, true, false)
	serve(h, "GET", "/api/v1/jobs/job_secret_name?token=hunter2&password=abc", nil, map[string]string{"X-API-Key": testAPIKey})
	rec := serve(h, "GET", "/api/v1/health", nil, nil)
	out := buf.String()
	for _, leak := range []string{"hunter2", "job_secret_name", "password"} {
		if strings.Contains(out, leak) {
			t.Fatalf("access log leaked %q: %s", leak, out)
		}
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if m["msg"] != "http request" || m["route"] != "/api/v1/health" {
			continue
		}
		found = true
		if m["request_id"] != rec.Header().Get(RequestIDHeader) || m["method"] != "GET" || m["status"] != float64(200) ||
			m["client_ip"] == nil || m["bytes"] == nil || m["duration_ms"] == nil {
			t.Errorf("access log line = %v", m)
		}
	}
	if !found {
		t.Fatalf("no access log line for the health check: %s", out)
	}
}

func TestAccessLogOffByDefault(t *testing.T) {
	h, _, buf := observeServer(t, nil, false, false)
	serve(h, "GET", "/api/v1/health", nil, nil)
	if strings.Contains(buf.String(), `"http request"`) {
		t.Errorf("access log written while off: %s", buf.String())
	}
}

// TestRequestMetricsAreBoundedByRoutes sends many distinct unknown paths, methods
// and path parameters: the observed routes stay within the registered patterns.
func TestRequestMetricsAreBoundedByRoutes(t *testing.T) {
	h, got, _ := observeServer(t, nil, false, false)
	key := map[string]string{"X-API-Key": testAPIKey}
	for i := 0; i < 300; i++ {
		serve(h, "GET", fmt.Sprintf("/random/%d/%x", i, i*7919), nil, nil)
		serve(h, "GET", fmt.Sprintf("/api/v1/unknown-%d", i), nil, key)
		serve(h, "GET", fmt.Sprintf("/api/v1/jobs/job_%d", i), nil, key)
		serve(h, http.MethodPatch, fmt.Sprintf("/api/v1/jobs/%d", i), nil, key)
		serve(h, fmt.Sprintf("WEIRD%d", i), "/api/v1/health", nil, nil)
	}
	routes := map[string]bool{}
	for _, o := range *got {
		routes[o.route] = true
	}
	if len(routes) > 4 {
		t.Errorf("%d distinct routes observed for 1500 requests: %v", len(routes), routes)
	}
	for r := range routes {
		if strings.Contains(r, "job_") || strings.Contains(r, "unknown-") || strings.Contains(r, "random") {
			t.Errorf("a raw path became a route label: %q", r)
		}
	}
	if !routes[""] || !routes["/api/v1/jobs/{id}"] {
		t.Errorf("expected unmatched and the route pattern, got %v", routes)
	}
}

func TestMetricsScrapeNotObserved(t *testing.T) {
	h, got, _ := observeServer(t, nil, true, false)
	serve(h, "GET", "/metrics", nil, map[string]string{"X-API-Key": testAPIKey})
	for _, o := range *got {
		if o.route == "/metrics" {
			t.Errorf("a /metrics scrape was observed: %v", o)
		}
	}
}

func TestObserveHTTPRequestSeries(t *testing.T) {
	m := metrics.New(metrics.BuildInfo{})
	for i := 0; i < 1000; i++ {
		m.ObserveHTTPRequest("", fmt.Sprintf("M%d", i), 404, time.Millisecond)
	}
	m.ObserveHTTPRequest("/api/v1/jobs/{id}", "GET", 200, 20*time.Millisecond)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`mongorescue_http_requests_total{code="404",method="OTHER",route="unmatched"} 1000`,
		`mongorescue_http_requests_total{code="200",method="GET",route="/api/v1/jobs/{id}"} 1`,
		`mongorescue_http_request_duration_seconds_count{method="GET",route="/api/v1/jobs/{id}"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestLogLinesOfARequestCarryItsID(t *testing.T) {
	h, _, buf := observeServer(t, nil, true, false)
	rec := serve(h, "GET", "/api/v1/health", nil, nil)
	id := rec.Header().Get(RequestIDHeader)
	if !strings.Contains(buf.String(), id) {
		t.Errorf("no log line carries request id %s: %q", id, buf.String())
	}
}
