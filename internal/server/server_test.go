package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func setupTestServer(t *testing.T) (*Server, store.Store, *storage.MockStorage) {
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	cfg := bootConfig()

	// Mock runner for backup
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("backup-data"))), strings.NewReader("ok"), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(bRunner))

	// Mock runner for restore
	rRunner := func(_ context.Context, _ string, _ io.Reader, _ ...string) (io.Reader, func() error, error) {
		return strings.NewReader("ok"), func() error { return nil }, nil
	}
	rEngine := restore.NewEngine(mockStorage, "mongodb://localhost:27017", restore.WithRunner(rRunner))

	sched := scheduler.NewScheduler(metaStore, bEngine, mockStorage, nil)

	srv := NewServer(cfg, metaStore, bEngine, rEngine, mockStorage, sched, nil, nil, withTestConnection(t, metaStore, nil))
	return srv, metaStore, mockStorage
}

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	rec := httptest.NewRecorder()

	srv.buildRoutes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var res apiResponse
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !res.Success {
		t.Error("expected success true in response")
	}
}

func TestJobsEndpoints(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	mux := srv.buildRoutes()

	// 1. Create Job
	newJob := models.Job{
		Name:           "Nightly Analytics",
		Database:       "analytics_prod",
		CronExpression: "@daily",
		RetentionDays:  14,
		RetentionCount: 7,
		Enabled:        true,
		ConnectionID:   testConnID,
	}
	body, _ := json.Marshal(newJob)
	createReq := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewReader(body))
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d (body: %s)", createRec.Code, createRec.Body.String())
	}

	// 2. List Jobs
	listReq := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", listRec.Code)
	}

	// 3. Delete Job
	var listRes apiResponse
	_ = json.NewDecoder(listRec.Body).Decode(&listRes)
	dataSlice, _ := listRes.Data.([]any)
	if len(dataSlice) == 0 {
		t.Fatal("expected at least 1 job in list")
	}
	jobMap := dataSlice[0].(map[string]any)
	jobID := jobMap["id"].(string)

	delReq := httptest.NewRequest("DELETE", "/api/v1/jobs/"+jobID, nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on delete, got %d", delRec.Code)
	}
}

func TestBackupsAndRestoreEndpoints(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	mux := srv.buildRoutes()

	// 1. Trigger On-Demand Backup
	backupOpts := models.BackupOptions{
		Database:     "users_db",
		ConnectionID: testConnID,
	}
	body, _ := json.Marshal(backupOpts)
	bkpReq := httptest.NewRequest("POST", "/api/v1/backups", bytes.NewReader(body))
	bkpRec := httptest.NewRecorder()
	mux.ServeHTTP(bkpRec, bkpReq)

	backupID := acceptedID(t, bkpRec)
	if got := awaitRecord(t, mux, "/api/v1/backups", backupID); got["status"] != "completed" {
		t.Fatalf("backup did not complete: %v", got)
	}

	// 2. Trigger Disaster Recovery Restore
	restoreReq := models.RestoreRequest{
		BackupID: backupID,
	}
	rstBody, _ := json.Marshal(restoreReq)
	rstReq := httptest.NewRequest("POST", "/api/v1/restore", bytes.NewReader(rstBody))
	rstRec := httptest.NewRecorder()
	mux.ServeHTTP(rstRec, rstReq)

	if got := awaitRecord(t, mux, "/api/v1/restores", acceptedID(t, rstRec)); got["status"] != "completed" {
		t.Fatalf("restore did not complete: %v", got)
	}

	// 3. Stats Endpoint
	statsReq := httptest.NewRequest("GET", "/api/v1/stats", nil)
	statsRec := httptest.NewRecorder()
	mux.ServeHTTP(statsRec, statsReq)

	if statsRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on stats, got %d", statsRec.Code)
	}
}

// newChainServer builds a Server through NewServer with an embedded static FS and
// returns its fully composed handler (logging -> CORS -> auth -> mux). When mutate
// sets no API key, requests without credentials are authenticated with testAPIKey.
func newChainServer(t *testing.T, mutate func(*testConfig)) (http.Handler, store.Store) {
	t.Helper()
	h, metaStore, _ := newChainServerWithStorage(t, mutate)
	return h, metaStore
}

// newChainServerWithStorage is newChainServer that also exposes the mock storage driver.
func newChainServerWithStorage(t *testing.T, mutate func(*testConfig)) (http.Handler, store.Store, *storage.MockStorage) {
	t.Helper()
	srv, metaStore, mockStorage := setupTestServer(t)
	cfg := newTestConfig()
	if mutate != nil {
		mutate(cfg)
	}
	autoKey := cfg.APIKey == ""
	if autoKey {
		cfg.APIKey = testAPIKey
	}
	staticFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>dashboard</html>")}}
	full := NewServer(bootConfig(), metaStore, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, staticFS, nil,
		WithConnections(srv.connections), WithAuth(newTestAuth(t, metaStore.(auth.Repository), cfg.APIKey)),
		WithSettings(newTestSettings(t, metaStore.(settings.Repository), cfg.Security)))
	if autoKey {
		return keyed{full.Handler()}, metaStore, mockStorage
	}
	return full.Handler(), metaStore, mockStorage
}

func serve(h http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthChain(t *testing.T) {
	const key = "super-secret-token"
	h, _ := newChainServer(t, func(c *testConfig) { c.APIKey = key })

	tests := []struct {
		name    string
		method  string
		path    string
		body    []byte
		headers map[string]string
		want    int
	}{
		{name: "no key", method: "GET", path: "/api/v1/jobs", want: http.StatusUnauthorized},
		{name: "wrong bearer", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Authorization": "Bearer wrong"}, want: http.StatusUnauthorized},
		{name: "wrong x-api-key", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"X-API-Key": "wrong"}, want: http.StatusUnauthorized},
		{name: "key prefix only", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"X-API-Key": key[:5]}, want: http.StatusUnauthorized},
		{name: "empty bearer", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Authorization": "Bearer "}, want: http.StatusUnauthorized},
		{name: "whitespace bearer", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Authorization": "Bearer    "}, want: http.StatusUnauthorized},
		{name: "correct bearer", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Authorization": "Bearer " + key}, want: http.StatusOK},
		{name: "correct x-api-key", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"X-API-Key": key}, want: http.StatusOK},
		{name: "health is public", method: "GET", path: "/api/v1/health", want: http.StatusOK},
		{name: "setup status is public", method: "GET", path: "/api/v1/setup/status", want: http.StatusOK},
		{name: "login is public", method: "POST", path: "/api/v1/auth/login", body: []byte(`{"username":"x","password":"y"}`), headers: map[string]string{"Content-Type": "application/json"}, want: http.StatusUnauthorized},
		{name: "static root is public", method: "GET", path: "/", want: http.StatusOK},
		{name: "me answers signed-out visitors", method: "GET", path: "/api/v1/auth/me", want: http.StatusOK},
		{name: "users protected", method: "GET", path: "/api/v1/users", want: http.StatusUnauthorized},
		{name: "api keys protected", method: "GET", path: "/api/v1/api-keys", want: http.StatusUnauthorized},
		{name: "connections protected", method: "GET", path: "/api/v1/connections", want: http.StatusUnauthorized},
		{name: "connection test protected", method: "POST", path: "/api/v1/connections/test", body: []byte(`{}`), want: http.StatusUnauthorized},
		{name: "unknown api path protected", method: "GET", path: "/api/v1/nope", want: http.StatusUnauthorized},
		{name: "bogus session cookie", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Cookie": SessionCookieName + "=forged"}, want: http.StatusUnauthorized},
		{name: "lowercase bearer scheme", method: "GET", path: "/api/v1/jobs", headers: map[string]string{"Authorization": "bearer " + key}, want: http.StatusOK},
		{name: "POST jobs protected", method: "POST", path: "/api/v1/jobs", body: []byte(`{"database":"x"}`), want: http.StatusUnauthorized},
		{name: "POST backups protected", method: "POST", path: "/api/v1/backups", body: []byte(`{"database":"x"}`), want: http.StatusUnauthorized},
		{name: "POST restore protected", method: "POST", path: "/api/v1/restore", body: []byte(`{}`), want: http.StatusUnauthorized},
		{name: "POST trigger protected", method: "POST", path: "/api/v1/jobs/job_1/run", want: http.StatusUnauthorized},
		{name: "DELETE job protected", method: "DELETE", path: "/api/v1/jobs/job_1", want: http.StatusUnauthorized},
		{name: "DELETE backup protected", method: "DELETE", path: "/api/v1/backups/bkp_1", want: http.StatusUnauthorized},
		{name: "config protected", method: "GET", path: "/api/v1/config", want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(h, tt.method, tt.path, tt.body, tt.headers)
			if rec.Code != tt.want {
				t.Fatalf("%s %s: expected %d, got %d (body: %s)", tt.method, tt.path, tt.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestNoUnauthenticatedMode proves that without any API key configured the API still
// requires a session: there is no permissive mode anymore.
func TestNoUnauthenticatedMode(t *testing.T) {
	srv, metaStore, _ := setupTestServer(t)
	cfg := bootConfig() // no API key
	full := NewServer(cfg, metaStore, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(newTestAuth(t, metaStore.(auth.Repository), "")))
	h := full.Handler()

	for _, path := range []string{"/api/v1/jobs", "/api/v1/backups", "/api/v1/settings", "/api/v1/storage-targets", "/api/v1/connections"} {
		if rec := serve(h, "GET", path, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials: %d; want 401", path, rec.Code)
		}
	}
	if rec := serve(h, "GET", "/api/v1/jobs", nil, map[string]string{"Authorization": "Bearer "}); rec.Code != http.StatusUnauthorized {
		t.Errorf("empty bearer with no static key: %d; want 401", rec.Code)
	}
	// A server without an auth service fails closed as well.
	bare := NewServer(cfg, metaStore, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil)
	if rec := serve(bare.Handler(), "GET", "/api/v1/jobs", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("server without auth service: %d; want 401", rec.Code)
	}
}

func TestCORSDefaultOff(t *testing.T) {
	h, _ := newChainServer(t, nil)
	origin := map[string]string{"Origin": "https://evil.example.com"}

	rec := serve(h, "GET", "/api/v1/health", nil, origin)
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Fatalf("expected no CORS headers by default, got %s=%q", k, rec.Header().Get(k))
		}
	}

	pre := serve(h, "OPTIONS", "/api/v1/backups", nil, map[string]string{
		"Origin":                        "https://evil.example.com",
		"Access-Control-Request-Method": "POST",
	})
	if pre.Code == http.StatusNoContent {
		t.Fatal("preflight must not be special-cased when CORS is disabled")
	}
	if pre.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("preflight must not receive CORS headers when CORS is disabled")
	}
}

func TestCORSAllowList(t *testing.T) {
	const allowed = "https://ops.example.com"
	h, _ := newChainServer(t, func(c *testConfig) {
		c.Security.CORSOrigins = []string{allowed}
		c.APIKey = "cors-test-api-key-0123"
	})

	t.Run("allowed origin reflected", func(t *testing.T) {
		rec := serve(h, "GET", "/api/v1/health", nil, map[string]string{"Origin": allowed})
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowed {
			t.Fatalf("expected reflected origin %q, got %q", allowed, got)
		}
		if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Origin") {
			t.Fatalf("expected Vary: Origin, got %v", rec.Header().Values("Vary"))
		}
	})

	t.Run("allowed preflight answered 204 without auth", func(t *testing.T) {
		rec := serve(h, "OPTIONS", "/api/v1/backups", nil, map[string]string{
			"Origin":                        allowed,
			"Access-Control-Request-Method": "POST",
		})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204 for allowed preflight, got %d", rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
			t.Fatalf("expected allow methods, got %q", rec.Header().Get("Access-Control-Allow-Methods"))
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Fatalf("expected allow headers, got %q", rec.Header().Get("Access-Control-Allow-Headers"))
		}
	})

	t.Run("disallowed origin gets none", func(t *testing.T) {
		for _, o := range []string{"https://evil.example.com", allowed + ".evil.com", "null", "*"} {
			rec := serve(h, "OPTIONS", "/api/v1/backups", nil, map[string]string{"Origin": o})
			for k := range rec.Header() {
				if strings.HasPrefix(k, "Access-Control-") {
					t.Fatalf("origin %q: unexpected CORS header %s=%q", o, k, rec.Header().Get(k))
				}
			}
			if rec.Code == http.StatusNoContent {
				t.Fatalf("origin %q: preflight must not succeed", o)
			}
		}
	})
}

func TestSaveJobRejectsInvalidID(t *testing.T) {
	h, metaStore := newChainServer(t, nil)

	for _, id := range []string{"x');alert(1);//", "<img src=x>", "a b", strings.Repeat("a", 65)} {
		body, _ := json.Marshal(models.Job{ID: id, Database: "db", CronExpression: "@daily"})
		rec := serve(h, "POST", "/api/v1/jobs", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("id %q: expected 400, got %d (body: %s)", id, rec.Code, rec.Body.String())
		}
	}

	jobs, _ := metaStore.ListJobs(context.Background())
	if len(jobs) != 0 {
		t.Fatalf("expected no jobs persisted, got %d", len(jobs))
	}

	// Action paths are lookup-based: an unknown ID is simply not found.
	if rec := serve(h, "POST", "/api/v1/jobs/x');alert(1);%2F%2F/run", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("trigger with unknown invalid id: expected 404, got %d", rec.Code)
	}
	restoreBody, _ := json.Marshal(models.RestoreRequest{BackupID: "x');alert(1);//"})
	if rec := serve(h, "POST", "/api/v1/restore", restoreBody, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("restore with unknown invalid backup id: expected 404, got %d", rec.Code)
	}
}

func TestSaveJobGeneratesSafeIDFromDatabase(t *testing.T) {
	h, _ := newChainServer(t, nil)

	body, _ := json.Marshal(models.Job{Database: "Evil DB');<script>" + strings.Repeat("x", 80), CronExpression: "@daily", ConnectionID: testConnID})
	rec := serve(h, "POST", "/api/v1/jobs", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var res struct {
		Data models.Job `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := models.ValidateID(res.Data.ID); err != nil {
		t.Fatalf("generated id %q is invalid: %v", res.Data.ID, err)
	}
}

func TestLegacyIDsRemainActionable(t *testing.T) {
	h, metaStore, mockStorage := newChainServerWithStorage(t, nil)
	ctx := context.Background()

	legacyJob := &models.Job{ID: "job_a+b_1700000000", Name: "legacy", Database: "a+b", CronExpression: "@daily", ConnectionID: testConnID}
	if err := metaStore.SaveJob(ctx, legacyJob); err != nil {
		t.Fatalf("seed legacy job: %v", err)
	}
	if models.ValidateID(legacyJob.ID) == nil {
		t.Fatal("test precondition: legacy job id should fail the strict pattern")
	}

	// Trigger by legacy ID (path-escaped '+').
	awaitRecord(t, h, "/api/v1/backups", acceptedID(t, serve(h, "POST", "/api/v1/jobs/job_a%2Bb_1700000000/run", nil, nil)))

	// Update an existing legacy job.
	legacyJob.Name = "renamed"
	body, _ := json.Marshal(legacyJob)
	if rec := serve(h, "POST", "/api/v1/jobs", body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("update legacy job: expected 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if stored, _ := metaStore.GetJob(ctx, legacyJob.ID); stored == nil || stored.Name != "renamed" {
		t.Fatalf("legacy job update not persisted: %+v", stored)
	}

	// Restore from a legacy backup ID.
	const legacyBackupID = "bkp_café_20260101_000000"
	srvStorageKey := "café/legacy.archive.gz"
	legacyBackup := &models.BackupRecord{ID: legacyBackupID, Database: "café", Status: models.StatusCompleted, StorageKey: srvStorageKey}
	if err := metaStore.SaveBackupRecord(ctx, legacyBackup); err != nil {
		t.Fatalf("seed legacy backup: %v", err)
	}
	if _, err := mockStorage.Save(ctx, srvStorageKey, strings.NewReader("archive")); err != nil {
		t.Fatalf("seed legacy artifact: %v", err)
	}
	// Legacy backups have no connection: the target must be named.
	rstBody, _ := json.Marshal(models.RestoreRequest{BackupID: legacyBackupID, DryRun: true})
	if rec := serve(h, "POST", "/api/v1/restore", rstBody, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "target_connection_id") {
		t.Fatalf("restore of a backup without connection and no target: %d %s; want 400", rec.Code, rec.Body.String())
	}
	rstBody, _ = json.Marshal(models.RestoreRequest{BackupID: legacyBackupID, DryRun: true, TargetConnectionID: testConnID})
	if got := awaitRecord(t, h, "/api/v1/restores", acceptedID(t, serve(h, "POST", "/api/v1/restore", rstBody, nil))); got["status"] != "completed" {
		t.Fatalf("restore legacy backup did not complete: %v", got)
	}

	// New jobs must still satisfy the strict pattern.
	newBody, _ := json.Marshal(models.Job{ID: "job_new+x", Database: "db", CronExpression: "@daily", ConnectionID: testConnID})
	if rec := serve(h, "POST", "/api/v1/jobs", newBody, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("new invalid id: expected 400, got %d", rec.Code)
	}
}

func TestJobCreatesInTheSameSecondNeverOverwriteEachOther(t *testing.T) {
	h, metaStore := newChainServer(t, nil)
	ids := map[string]bool{}
	for i := range 5 {
		body, _ := json.Marshal(models.Job{Name: fmt.Sprintf("job %d", i), Database: "shop", CronExpression: "@daily", ConnectionID: testConnID})
		rec := serve(h, "POST", "/api/v1/jobs", body, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, rec.Code, rec.Body)
		}
		var res struct {
			Data models.Job `json:"data"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&res)
		if err := models.ValidateID(res.Data.ID); err != nil || ids[res.Data.ID] {
			t.Fatalf("generated id %q is invalid or reused (%v)", res.Data.ID, err)
		}
		ids[res.Data.ID] = true
	}
	jobs, _ := metaStore.ListJobs(context.Background())
	if len(jobs) != 5 {
		t.Fatalf("%d jobs stored; want 5", len(jobs))
	}
	// Updating a job deleted meanwhile does not recreate it.
	var gone string
	for id := range ids {
		gone = id
		break
	}
	if err := metaStore.DeleteJob(context.Background(), gone); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(models.Job{ID: gone, Name: "late", Database: "shop", CronExpression: "@daily", ConnectionID: testConnID})
	if rec := serve(h, "POST", "/api/v1/jobs", body, nil); rec.Code != http.StatusCreated {
		// An unknown (valid) id is a create with that id, not a resurrection of history.
		t.Fatalf("create with a free id: %d %s", rec.Code, rec.Body)
	}
}
