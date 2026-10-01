package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/integrity"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// oneTarget serves a single storage target "tgt_local" backed by mem.
type oneTarget struct{ mem *storage.MockStorage }

func (o oneTarget) List(context.Context) ([]*models.StorageTarget, error) {
	return []*models.StorageTarget{{ID: "tgt_local", Name: "Local", Type: models.StorageLocal}}, nil
}

func (o oneTarget) Resolve(_ context.Context, id string) (*models.StorageTarget, error) {
	if id != "" && id != "tgt_local" {
		return nil, store.ErrNotFound
	}
	return &models.StorageTarget{ID: "tgt_local", Name: "Local", Type: models.StorageLocal}, nil
}

func (o oneTarget) Storage(context.Context, string) (storage.Storage, error) { return o.mem, nil }

// integrityServer is a server with an integrity service on a temporary store.
func integrityServer(t *testing.T) (http.Handler, *store.SQLiteStore, *storage.MockStorage, *runs.Manager) {
	t.Helper()
	st := storetest.New(t)
	mem := storage.NewMockStorage()
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	svc := integrity.New(integrity.Config{Store: st, Targets: oneTarget{mem}, Runs: manager})
	srv := NewServer(bootConfig(), st, backup.NewEngine(mem, ""), restore.NewEngine(mem, ""), mem, nil, nil, nil,
		withTestConnection(t, st, nil), WithIntegrity(svc), WithRunManager(manager))
	admin := &auth.Principal{User: &auth.User{ID: "usr_1", Username: "alice"}, Method: auth.MethodSession, Scope: auth.ScopeAdmin}
	return asPrincipal(srv.buildRoutes(), admin), st, mem, manager
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, json.RawMessage) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, r))
	var res struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Error != "" {
		return rec.Code, json.RawMessage(`"` + res.Error + `"`)
	}
	return rec.Code, res.Data
}

func seedArchive(t *testing.T, st *store.SQLiteStore, mem *storage.MockStorage, id string, data []byte) *models.BackupRecord {
	t.Helper()
	key := "shop/2026/09/" + id + ".archive.gz"
	if _, err := mem.Save(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	done := time.Now().UTC().Add(-time.Hour)
	rec := &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_local",
		StorageKey: key, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), StartedAt: done.Add(-time.Minute), CompletedAt: &done}
	if err := st.SaveBackupRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func waitRuns(t *testing.T, m *runs.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Active()) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPinBlocksDeleteUntilUnpinned(t *testing.T) {
	h, st, mem, _ := integrityServer(t)
	rec := seedArchive(t, st, mem, "bkp_pin", []byte("archive"))

	code, data := call(t, h, http.MethodPost, "/api/v1/backups/bkp_pin/pin", `{"note":"legal hold"}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"pinned":true`) || !strings.Contains(string(data), `"pinned_by":"alice"`) {
		t.Fatalf("pin = %d %s", code, data)
	}
	if code, data = call(t, h, http.MethodDelete, "/api/v1/backups/bkp_pin", ""); code != http.StatusConflict || !strings.Contains(string(data), "unpin") {
		t.Fatalf("delete pinned = %d %s", code, data)
	}
	if _, err := mem.Stat(context.Background(), rec.StorageKey); err != nil {
		t.Fatalf("the archive of a pinned backup must stay: %v", err)
	}
	if code, _ = call(t, h, http.MethodPost, "/api/v1/backups/bkp_pin/unpin", ""); code != http.StatusOK {
		t.Fatalf("unpin = %d", code)
	}
	if code, _ = call(t, h, http.MethodDelete, "/api/v1/backups/bkp_pin", ""); code != http.StatusOK {
		t.Fatalf("delete after unpin = %d", code)
	}
	if code, _ = call(t, h, http.MethodPost, "/api/v1/backups/nope/pin", ""); code != http.StatusNotFound {
		t.Fatalf("pin unknown = %d", code)
	}
	if code, _ = call(t, h, http.MethodPost, "/api/v1/backups/bkp_pin/pin", "{"); code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", code)
	}
}

func TestVerifyAndIntegrityEndpoints(t *testing.T) {
	h, st, mem, manager := integrityServer(t)
	seedArchive(t, st, mem, "bkp_v", []byte("archive"))

	if code, data := call(t, h, http.MethodPost, "/api/v1/backups/bkp_v/verify", ""); code != http.StatusAccepted {
		t.Fatalf("verify = %d %s", code, data)
	}
	waitRuns(t, manager)
	if rec, _ := st.GetBackupRecord(context.Background(), "bkp_v"); rec.Verification != models.VerificationOK {
		t.Fatalf("after verify: %+v", rec)
	}
	failed := &models.BackupRecord{ID: "bkp_f", Database: "shop", Status: models.StatusFailed}
	_ = st.SaveBackupRecord(context.Background(), failed)
	if code, _ := call(t, h, http.MethodPost, "/api/v1/backups/bkp_f/verify", ""); code != http.StatusConflict {
		t.Fatalf("verify failed backup = %d", code)
	}
	if code, _ := call(t, h, http.MethodPost, "/api/v1/backups/nope/verify", ""); code != http.StatusNotFound {
		t.Fatalf("verify unknown = %d", code)
	}

	// Storage scan and orphan import.
	if _, err := mem.Save(context.Background(), "shop/2026/09/bkp_shop_20260901_030000_aaaa.archive.gz", strings.NewReader("orphan")); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, h, http.MethodGet, "/api/v1/storage-targets/tgt_local/scan", ""); code != http.StatusNotFound {
		t.Fatalf("scan before any = %d", code)
	}
	code, data := call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/scan", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"orphan_count":1`) {
		t.Fatalf("scan = %d %s", code, data)
	}
	if code, data = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/import", `{"key":"shop/2026/09/bkp_shop_20260901_030000_aaaa.archive.gz"}`); code != http.StatusAccepted {
		t.Fatalf("import = %d %s", code, data)
	}
	waitRuns(t, manager)
	if code, _ = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/import", `{"key":"notes.txt"}`); code != http.StatusConflict {
		t.Fatalf("import non-archive = %d", code)
	}
	if code, _ = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/import", `{}`); code != http.StatusBadRequest {
		t.Fatalf("import without key = %d", code)
	}
	if code, data = call(t, h, http.MethodGet, "/api/v1/integrity", ""); code != http.StatusOK || !strings.Contains(string(data), `"scans"`) {
		t.Fatalf("status = %d %s", code, data)
	}
	if code, _ = call(t, h, http.MethodPost, "/api/v1/integrity/sweep", ""); code != http.StatusAccepted {
		t.Fatalf("sweep = %d", code)
	}
	waitRuns(t, manager)
	if code, _ = call(t, h, http.MethodPost, "/api/v1/jobs/nope/restore-test", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("restore test without the engine wiring = %d", code)
	}
}

func TestRetentionPreviewEndpoint(t *testing.T) {
	h, st, _, _ := integrityServer(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_p", Name: "p", Database: "shop", CronExpression: "@daily", RetentionCount: 1}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, id := range []string{"bkp_new", "bkp_old"} {
		_ = st.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, JobID: job.ID, Trigger: models.TriggerScheduled, Database: "shop",
			Status: models.StatusCompleted, StartedAt: now.Add(-time.Duration(i+2) * 24 * time.Hour)})
	}
	code, data := call(t, h, http.MethodGet, "/api/v1/jobs/job_p/retention/preview", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"id":"bkp_old"`) || strings.Contains(string(data), `"id":"bkp_new"`) {
		t.Fatalf("preview = %d %s", code, data)
	}
	if code, data = call(t, h, http.MethodGet, "/api/v1/jobs/job_p/retention/preview?retention_count=5", ""); code != http.StatusOK || !strings.Contains(string(data), `"delete":[]`) {
		t.Fatalf("override preview = %d %s", code, data)
	}
	if code, _ = call(t, h, http.MethodGet, "/api/v1/jobs/job_p/retention/preview?retention_days=x", ""); code != http.StatusBadRequest {
		t.Fatalf("bad query = %d", code)
	}
	if code, _ = call(t, h, http.MethodGet, "/api/v1/jobs/nope/retention/preview", ""); code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", code)
	}
	if code, data = call(t, h, http.MethodGet, "/api/v1/jobs/job_p/retention/log", ""); code != http.StatusOK || string(data) != "[]" {
		t.Fatalf("log = %d %s", code, data)
	}
	if code, data = call(t, h, http.MethodGet, "/api/v1/jobs/job_p/restore-tests", ""); code != http.StatusOK || string(data) != "[]" {
		t.Fatalf("restore tests = %d %s", code, data)
	}
}
