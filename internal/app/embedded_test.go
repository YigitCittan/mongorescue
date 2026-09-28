package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppDashboardIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		dashboard bool
		want      int
	}{
		{false, http.StatusNotFound},
		{true, http.StatusOK},
	} {
		cfg := testConfig(t)
		cfg.Dashboard = tc.dashboard
		application, err := New(cfg, nil, WithGetenv(noEnv))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		h := application.Handler()

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != tc.want {
			t.Errorf("dashboard=%v: GET / = %d; want %d", tc.dashboard, rec.Code, tc.want)
		}
		if tc.dashboard && !strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("dashboard=%v: GET / did not serve index.html", tc.dashboard)
		}

		// The API works either way.
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("dashboard=%v: GET /api/v1/health = %d; want 200", tc.dashboard, rec.Code)
		}
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppStartStopWithoutListener(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	application.Stop() // no-op before Start
	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := application.Start(context.Background()); !errors.Is(err, ErrStarted) {
		t.Fatalf("second Start = %v; want ErrStarted", err)
	}
	if application.SetupCode() == "" {
		t.Fatal("fresh data directory must be in setup mode")
	}
	rec := httptest.NewRecorder()
	application.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/setup/status = %d", rec.Code)
	}
	application.Stop()
	application.Stop() // idempotent
	if err := application.Start(context.Background()); !errors.Is(err, ErrStarted) {
		t.Fatalf("Start after Stop = %v; want ErrStarted", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
