package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestRestorePreflightEndpoint(t *testing.T) {
	f := newRestoreSafetyFixture(t)
	body := `{"backup_id":"` + f.backupID + `","safe_clone":false,"target_database":"shop"}`
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/restores/preflight", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s", rec.Code, rec.Body)
	}
	var res struct {
		Success bool                   `json:"success"`
		Data    models.PreflightResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// Without an inspector the server checks are warnings; nothing was started.
	if !res.Success || !res.Data.OK || len(res.Data.Checks) == 0 || res.Data.Check(models.PreflightCheckConnection) == nil {
		t.Fatalf("preflight = %+v", res)
	}
	if f.restores.Load() != 0 {
		t.Fatal("a preflight ran mongorestore")
	}

	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/restores/preflight", strings.NewReader(`{"backup_id":"bkp_missing"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("preflight of a missing backup = %d; want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/restores/preflight", strings.NewReader(`{`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preflight of invalid JSON = %d; want 400", rec.Code)
	}
}

func TestRefusingPreflightAnswersConflictWithItsChecks(t *testing.T) {
	s := &Server{logger: slog.New(slog.DiscardHandler)}
	result := &models.PreflightResult{}
	result.Add(models.PreflightCheckDiskSpace, models.PreflightFail, "the target has 1 B free, less than the 1.0 KiB archive")
	rec := httptest.NewRecorder()
	s.writeOperationError(rec, &operations.PreflightError{Result: result})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", rec.Code)
	}
	var res struct {
		Success bool                   `json:"success"`
		Error   string                 `json:"error"`
		Data    models.PreflightResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Success || res.Data.OK || res.Data.Check(models.PreflightCheckDiskSpace) == nil || !strings.Contains(res.Error, "force") {
		t.Fatalf("response = %+v", res)
	}
}
