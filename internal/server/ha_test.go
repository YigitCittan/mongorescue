package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// testFence is an HA fence that passes the first passes checks and fails after.
type testFence struct{ passes atomic.Int64 }

// Check implements models.Fence.
func (f *testFence) Check() error {
	if f.passes.Add(-1) >= 0 {
		return nil
	}
	return errors.New("ha: not the active instance")
}

// Epoch implements models.Fence.
func (f *testFence) Epoch() uint64 { return 1 }

// setupFencedServer is setupTestServer with the HA fence fence, and a completed
// backup fd_1 whose archive is stored.
func setupFencedServer(t *testing.T, fence models.Fence) (*Server, store.Store) {
	t.Helper()
	metaStore := storetest.New(t)
	mockStorage := storage.NewMockStorage()
	bRunner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("backup-data"))), strings.NewReader("ok"), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(bRunner))
	rRunner := func(_ context.Context, _ string, _ io.Reader, _ ...string) (io.Reader, func() error, error) {
		return strings.NewReader("ok"), func() error { return nil }, nil
	}
	rEngine := restore.NewEngine(mockStorage, "mongodb://localhost:27017", restore.WithRunner(rRunner))
	sched := scheduler.NewScheduler(metaStore, bEngine, mockStorage, nil)
	srv := NewServer(bootConfig(), metaStore, bEngine, rEngine, mockStorage, sched, nil, nil,
		withTestConnection(t, metaStore, nil), WithFence(fence))
	ctx := context.Background()
	if err := metaStore.SaveBackupRecord(ctx, &models.BackupRecord{ID: "fd_1", Database: "shop", Status: models.StatusCompleted,
		StorageKey: "shop/fd_1.archive.gz", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mockStorage.Save(ctx, "shop/fd_1.archive.gz", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	return srv, metaStore
}

// TestFenceRefusesChangesOfAnInstanceThatLostTheLease checks that once the HA fence
// fails, a request that may change something answers 503 with Retry-After and
// changes nothing, whether the fence fails when the request arrives (the gate) or
// only right before the commit (the operations service); reads are still served.
func TestFenceRefusesChangesOfAnInstanceThatLostTheLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		passes int64
	}{
		{"at the gate", 0},
		{"before the commit", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence := &testFence{}
			fence.passes.Store(tc.passes)
			srv, st := setupFencedServer(t, fence)
			h := srv.fenceGate(asPrincipal(srv.mux, auth.SystemPrincipal()))
			rec := serve(h, "DELETE", "/api/v1/backups/fd_1?reason=cleanup", nil, nil)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "HA lease") {
				t.Fatalf("delete without the lease = %d %s (Retry-After %q); want 503 naming the HA lease", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
			}
			if b, err := st.GetBackupRecord(context.Background(), "fd_1"); err != nil || b.Status != models.StatusCompleted {
				t.Fatalf("the refused delete changed the backup: %+v, %v", b, err)
			}
			fence.passes.Store(0)
			if rec = serve(h, "GET", "/api/v1/backups", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fd_1") {
				t.Fatalf("a read without the lease = %d %s; want 200", rec.Code, rec.Body)
			}
		})
	}
	t.Run("with the lease", func(t *testing.T) {
		fence := &testFence{}
		fence.passes.Store(1 << 30)
		srv, _ := setupFencedServer(t, fence)
		h := srv.fenceGate(asPrincipal(srv.mux, auth.SystemPrincipal()))
		if rec := serve(h, "DELETE", "/api/v1/backups/fd_1?reason=cleanup", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("delete with the lease = %d %s; want 200", rec.Code, rec.Body)
		}
	})
}

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
