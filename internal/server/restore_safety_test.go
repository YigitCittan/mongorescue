package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// restoreSafetyFixture serves the API with a mongorestore stand-in that counts runs.
type restoreSafetyFixture struct {
	mux      http.Handler
	restores atomic.Int32
	backupID string
}

func newRestoreSafetyFixture(t *testing.T) *restoreSafetyFixture {
	t.Helper()
	f := &restoreSafetyFixture{}
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("backup-data")), strings.NewReader(""), func() error { return nil }, nil
	}
	rRunner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		f.restores.Add(1)
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(bRunner))
	rEngine := restore.NewEngine(mockStorage, "mongodb://localhost:27017", restore.WithRunner(rRunner))
	sched := scheduler.NewScheduler(metaStore, bEngine, mockStorage, nil)
	// In-place restores need admin; the fixture acts as the administrator.
	f.mux = asPrincipal(NewServer(bootConfig(), metaStore, bEngine, rEngine, mockStorage, sched, nil, nil,
		withTestConnection(t, metaStore, nil)).buildRoutes(), auth.SystemPrincipal())

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/backups", strings.NewReader(`{"connection_id":"conn_test","database":"shop"}`)))
	f.backupID = acceptedID(t, rec)
	awaitRecord(t, f.mux, "/api/v1/backups", f.backupID)
	return f
}

func (f *restoreSafetyFixture) restore(t *testing.T, extra string) (int, models.RestoreRecord, string) {
	t.Helper()
	body := `{"backup_id":"` + f.backupID + `"` + extra + `}`
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/restore", bytes.NewReader([]byte(body))))
	var res struct {
		Data  models.RestoreRecord `json:"data"`
		Error string               `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted {
		return rec.Code, res.Data, res.Error
	}
	// Accepted restores run in the background: report the finished record.
	final := awaitRecord(t, f.mux, "/api/v1/restores", res.Data.ID)
	raw, _ := json.Marshal(final)
	var done models.RestoreRecord
	_ = json.Unmarshal(raw, &done)
	return http.StatusOK, done, done.ErrorMessage
}

func TestRestoreOmittedSafeCloneDefaultsToClone(t *testing.T) {
	f := newRestoreSafetyFixture(t)
	code, rst, errMsg := f.restore(t, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", code, errMsg)
	}
	if !strings.HasPrefix(rst.TargetDatabase, "shop_rescue_") {
		t.Fatalf("omitted safe_clone must restore into a clone, got target %q", rst.TargetDatabase)
	}
	if f.restores.Load() != 1 {
		t.Fatalf("expected one mongorestore run, got %d", f.restores.Load())
	}
}

func TestRestoreInPlaceRequiresConfirmation(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{"safe_clone false without confirm", `,"safe_clone":false`},
		{"safe_clone false with confirm false", `,"safe_clone":false,"confirm_in_place":false`},
		{"target_database without safe_clone false", `,"target_database":"shop","confirm_in_place":true`},
		{"target_database with safe_clone true", `,"safe_clone":true,"target_database":"shop"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreSafetyFixture(t)
			code, _, errMsg := f.restore(t, tc.extra)
			if code != http.StatusBadRequest || !strings.Contains(errMsg, "confirm_in_place") {
				t.Fatalf("expected 400 mentioning confirm_in_place, got %d (%s)", code, errMsg)
			}
			if f.restores.Load() != 0 {
				t.Fatal("mongorestore must never run for an unconfirmed in-place restore")
			}
		})
	}
}

func TestRestoreInPlaceWithConfirmation(t *testing.T) {
	f := newRestoreSafetyFixture(t)
	code, rst, errMsg := f.restore(t, `,"safe_clone":false,"confirm_in_place":true`)
	if code != http.StatusOK || rst.TargetDatabase != "shop" {
		t.Fatalf("confirmed in-place restore: code=%d target=%q err=%s", code, rst.TargetDatabase, errMsg)
	}
	if !rst.Verified {
		t.Fatal("in-place restores are verified by default (auto policy)")
	}

	code, rst, errMsg = f.restore(t, `,"safe_clone":false,"confirm_in_place":true,"target_database":"shop_copy"`)
	if code != http.StatusOK || rst.TargetDatabase != "shop_copy" {
		t.Fatalf("confirmed custom target: code=%d target=%q err=%s", code, rst.TargetDatabase, errMsg)
	}
	if f.restores.Load() != 2 {
		t.Fatalf("expected two mongorestore runs, got %d", f.restores.Load())
	}
}
