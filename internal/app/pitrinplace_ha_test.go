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

// TestInPlacePITRInHAMode checks the wiring of in-place point-in-time restores in
// HA mode: the active instance has the swap ledger of the cluster and so gets past
// the HA guard (to the missing stream), through the API as well; the standby App
// has no ledger and keeps refusing them.
func TestInPlacePITRInHAMode(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	req := models.RestoreRequest{PITR: &models.PITRTarget{StreamID: "str_x", At: &at, InPlace: true, ConfirmInPlace: "shop"},
		Databases: []string{"shop"}}

	cfg := haTestConfig(t, t.TempDir(), haTestKey(t))
	sup := newTestSupervisor(t, cfg)
	sup.clock = newFakeClock().Now // the lease cannot expire however slowly the App opens
	standby, err := sup.open(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.bootstrap(context.Background(), standby); err != nil {
		t.Fatal(err)
	}
	if _, err = standby.ops.StartRestore(adminSession(), req); !errors.Is(err, operations.ErrInPlaceHAMode) {
		t.Fatalf("in place on the standby App: %v; want ErrInPlaceHAMode", err)
	}
	_ = standby.Close()
	// The active instance holds the lease, as in RunHA.
	runManager(t, sup.rt.mgr())
	active, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close() }()
	if _, err = active.ops.StartRestore(adminSession(), req); err == nil || errors.Is(err, operations.ErrInPlaceHAMode) {
		t.Fatalf("in place on the active instance: %v; want a refusal other than ErrInPlaceHAMode", err)
	}

	// Through the REST API: not the HA refusal either.
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
		if strings.Contains(rec.Body.String(), "swap ledger") {
			t.Fatalf("%s in HA mode: %d %s; the active instance must not refuse for the HA mode", path, rec.Code, rec.Body.String())
		}
	}
}
