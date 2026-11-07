package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/diskguard"
)

// fakeDisk is a disk guard whose Check returns err.
type fakeDisk struct{ err error }

func (f fakeDisk) Check() error { return f.err }

// readyReply is the decoded answer of GET /api/v1/ready.
type readyReply struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Code    string `json:"code"`
	Data    struct {
		Ready   bool   `json:"ready"`
		Reason  string `json:"reason"`
		Role    string `json:"role"`
		Cluster string `json:"cluster"`
	} `json:"data"`
}

// getReady serves GET /api/v1/ready through the whole handler chain, with no
// credentials.
func getReady(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, readyReply) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, readyPath, nil))
	var res readyReply
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("ready body %q: %v", rec.Body.String(), err)
	}
	return rec, res
}

// wantNotReady checks a 503 with the machine-readable reason.
func wantNotReady(t *testing.T, rec *httptest.ResponseRecorder, res readyReply, reason string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s); want 503 %s", rec.Code, rec.Body.String(), reason)
	}
	if res.Success || res.Code != reason || res.Data.Reason != reason || res.Data.Ready || res.Error == "" {
		t.Fatalf("body = %s; want code and data.reason %q", rec.Body.String(), reason)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 needs a Retry-After")
	}
}

func TestReadyIsOKOutsideHA(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.diskGuard = fakeDisk{}
	rec, res := getReady(t, srv.Handler())
	if rec.Code != http.StatusOK || !res.Success || !res.Data.Ready || res.Data.Role != HARoleActive || res.Data.Reason != "" {
		t.Fatalf("ready = %d %s; want 200, ready, role active", rec.Code, rec.Body.String())
	}
}

func TestReadyActiveWithAHealthyLease(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.haStatus = func() HAStatus { return HAStatus{Role: HARoleActive, Cluster: "prod", LeaseHealthy: true} }
	rec, res := getReady(t, srv.Handler())
	if rec.Code != http.StatusOK || !res.Data.Ready || res.Data.Role != HARoleActive || res.Data.Cluster != "prod" {
		t.Fatalf("ready = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadyNotActive(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	for _, role := range []string{HARoleStandby, HARoleSwitching} {
		srv.haStatus = func() HAStatus { return HAStatus{Role: role, Cluster: "prod", RetryAfterSeconds: 7} }
		rec, res := getReady(t, srv.Handler())
		wantNotReady(t, rec, res, ReadyNotActive)
		if res.Data.Role != role || res.Data.Cluster != "prod" || rec.Header().Get("Retry-After") != "7" {
			t.Errorf("role %s: %s (Retry-After %q)", role, rec.Body.String(), rec.Header().Get("Retry-After"))
		}
	}
}

// TestReadyOnAStandbyIsServedWithoutCredentials checks the standby gate lets the
// readiness check through, like the health check, while the API stays closed.
func TestReadyOnAStandbyIsServedWithoutCredentials(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.haStatus = func() HAStatus { return HAStatus{Role: HARoleStandby, Cluster: "prod"} }
	h := srv.Handler()
	rec, res := getReady(t, h)
	wantNotReady(t, rec, res, ReadyNotActive)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("a standby's health = %d; want 200 (liveness)", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, readyPath, nil))
	if rec.Code == http.StatusOK {
		t.Error("POST /api/v1/ready answered 200 on a standby")
	}
}

func TestReadyDraining(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.scheduler.Pause()
	rec, res := getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyDraining)

	// The health check stays 200 while draining: a liveness probe must not restart
	// the process during the wait.
	if code, data := getHealth(t, srv); code != http.StatusOK || data["draining"] != true {
		t.Fatalf("health while draining = %d %v", code, data)
	}
	srv.scheduler.Resume()
	if rec, res = getReady(t, srv.Handler()); rec.Code != http.StatusOK || !res.Data.Ready {
		t.Fatalf("ready after resume = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadyStoreUnwritable(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	closer, ok := metaStore.(interface{ Close() error })
	if !ok {
		t.Fatal("the test store cannot be closed")
	}
	if rec, res := getReady(t, srv.Handler()); rec.Code != http.StatusOK || !res.Data.Ready {
		t.Fatalf("ready with an open store = %d %s", rec.Code, rec.Body.String())
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	rec, res := getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyStoreUnwritable)
}

func TestReadyDiskFull(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.diskGuard = fakeDisk{err: errors.Join(diskguard.ErrLowSpace, errors.New("0 MiB free"))}
	rec, res := getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyDiskFull)
}

// TestReadyReasonOrder checks the first failing reason wins: a standby is not
// active whatever else is wrong.
func TestReadyReasonOrder(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.haStatus = func() HAStatus { return HAStatus{Role: HARoleStandby} }
	srv.diskGuard = fakeDisk{err: diskguard.ErrLowSpace}
	srv.scheduler.Pause()
	rec, res := getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyNotActive)

	srv.haStatus = func() HAStatus { return HAStatus{Role: HARoleActive} }
	rec, res = getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyDraining)

	srv.scheduler.Resume()
	rec, res = getReady(t, srv.Handler())
	wantNotReady(t, rec, res, ReadyDiskFull)
}

// TestReadyIsNeverRateLimited checks probes are not throttled like the other
// public routes.
func TestReadyIsNeverRateLimited(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := srv.Handler()
	for i := range 500 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, readyPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %s = %d", strconv.Itoa(i), rec.Code)
		}
	}
}

func TestSwitchingHandlerIsNotReady(t *testing.T) {
	h := SwitchingHandler(func() HAStatus { return HAStatus{Role: HARoleActive, Cluster: "prod"} }, "v1", nil)
	rec, res := getReady(t, h)
	wantNotReady(t, rec, res, ReadyNotActive)
	if res.Data.Role != HARoleSwitching || res.Data.Cluster != "prod" {
		t.Errorf("body = %s", rec.Body.String())
	}
}
