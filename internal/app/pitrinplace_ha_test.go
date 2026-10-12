package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// TestInPlacePITRIsRefusedInHAMode checks the wiring of the HA guard: the active
// instance of an active-passive cluster refuses an in-place point-in-time restore
// with 409 (a swap that outlasts the lease could overlap the next leader's
// startup), while a single instance does not refuse it for that reason.
func TestInPlacePITRIsRefusedInHAMode(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	req := models.RestoreRequest{PITR: &models.PITRTarget{StreamID: "str_x", At: &at, InPlace: true, ConfirmInPlace: "shop"},
		Databases: []string{"shop"}}

	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := newTestSupervisor(t, cfg)
	standby, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(context.Background(), standby); err != nil {
		t.Fatal(err)
	}
	_ = standby.Close()
	// The active instance holds the lease, as in RunHA.
	runManager(t, sup.rt.mgr())
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close() }()
	if _, err = active.ops.StartRestore(adminSession(), req); !errors.Is(err, operations.ErrInPlaceHAMode) {
		t.Fatalf("in place in HA mode: %v; want ErrInPlaceHAMode", err)
	}

	// Through the REST API: 409 with the reason.
	_, key, err := active.auth.CreateAPIKey(context.Background(), auth.SystemPrincipal(), "ha admin", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"pitr":{"stream_id":"str_x","at":"2026-10-05T12:00:00Z","in_place":true,"confirm_in_place":"shop"},"databases":["shop"]}`
	for _, path := range []string{"/api/v1/restore", "/api/v1/restores/preflight"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		active.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in-place PITR is not available in HA mode yet") {
			t.Fatalf("%s in HA mode: %d %s; want 409", path, rec.Code, rec.Body.String())
		}
	}

	// A single instance gets past the guard (to the missing stream).
	single, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = single.Close() }()
	if _, err = single.ops.StartRestore(adminSession(), req); err == nil || errors.Is(err, operations.ErrInPlaceHAMode) {
		t.Fatalf("in place without HA: %v; want another refusal", err)
	}
}
