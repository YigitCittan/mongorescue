package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// asyncFixture serves the API with a mongodump stand-in that blocks until released.
type asyncFixture struct {
	h       http.Handler
	store   store.Store
	runs    *runs.Manager
	release chan struct{}
	started chan struct{}
}

func newAsyncFixture(t *testing.T) *asyncFixture {
	t.Helper()
	f := &asyncFixture{release: make(chan struct{}), started: make(chan struct{}, 8), runs: runs.NewManager(nil)}
	metaStore := storetest.New(t)
	f.store = metaStore
	mockStorage := storage.NewMockStorage()
	bRunner := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		f.started <- struct{}{}
		pr, pw := io.Pipe()
		go func() {
			select {
			case <-f.release:
				_, _ = pw.Write([]byte("archive"))
				_ = pw.Close()
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
			}
		}()
		return pr, strings.NewReader(""), func() error { return ctx.Err() }, nil
	}
	rRunner := func(_ context.Context, _ string, stdin io.Reader, _ ...string) (io.Reader, func() error, error) {
		_, _ = io.Copy(io.Discard, stdin)
		return strings.NewReader(""), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(bRunner))
	rEngine := restore.NewEngine(mockStorage, "mongodb://localhost:27017", restore.WithRunner(rRunner))
	sched := scheduler.NewScheduler(metaStore, bEngine, mockStorage, nil)
	srv := NewServer(bootConfig(), metaStore, bEngine, rEngine, mockStorage, sched, nil, nil, WithRunManager(f.runs),
		withTestConnection(t, metaStore, nil))
	f.h = srv.buildRoutes()
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		_ = f.runs.Shutdown(context.Background())
	})
	return f
}

func (f *asyncFixture) post(ctx context.Context, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx))
	return rec
}

func (f *asyncFixture) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background backup never started")
	}
}

func TestManualBackupSurvivesClientDisconnect(t *testing.T) {
	f := newAsyncFixture(t)
	reqCtx, disconnect := context.WithCancel(context.Background())

	id := acceptedID(t, f.post(reqCtx, "/api/v1/backups", `{"connection_id":"conn_test","database":"shop"}`))
	f.waitStarted(t)
	disconnect() // the browser tab is closed while mongodump is still running
	close(f.release)

	got := awaitRecord(t, f.h, "/api/v1/backups", id)
	if got["status"] != "completed" {
		t.Fatalf("backup must complete after the client disconnected, got %v", got)
	}
}

func TestJobRunIsAsyncAndGuardedAgainstConcurrentRuns(t *testing.T) {
	f := newAsyncFixture(t)
	job := &models.Job{ID: "job_shop", Name: "shop", Database: "shop", CronExpression: "@daily", ConnectionID: testConnID}
	if err := f.store.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	id := acceptedID(t, f.post(context.Background(), "/api/v1/jobs/job_shop/run", ""))
	f.waitStarted(t)

	// The record is visible (in progress) while the run is going on.
	rec, err := f.store.GetBackupRecord(context.Background(), id)
	if err != nil || rec.Status != models.StatusInProgress || rec.JobID != "job_shop" {
		t.Fatalf("in-progress record not persisted: %+v, %v", rec, err)
	}

	for _, tc := range []struct{ path, body string }{
		{"/api/v1/jobs/job_shop/run", ""},
		{"/api/v1/backups", `{"connection_id":"conn_test","database":"shop"}`},
	} {
		if resp := f.post(context.Background(), tc.path, tc.body); resp.Code != http.StatusConflict {
			t.Fatalf("POST %s while running: expected 409, got %d (%s)", tc.path, resp.Code, resp.Body.String())
		}
	}
	// Another database is independent.
	other := acceptedID(t, f.post(context.Background(), "/api/v1/backups", `{"connection_id":"conn_test","database":"billing"}`))
	f.waitStarted(t)

	close(f.release)
	for _, bid := range []string{id, other} {
		if got := awaitRecord(t, f.h, "/api/v1/backups", bid); got["status"] != "completed" {
			t.Fatalf("backup %s: %v", bid, got)
		}
	}
	stored, _ := f.store.GetJob(context.Background(), "job_shop")
	if stored.LastRun == nil {
		t.Fatal("job LastRun not updated by the background run")
	}

	// Once finished, the job can run again. The final record is persisted just
	// before the run releases its busy guard, so allow a brief 409 window.
	f.release = make(chan struct{})
	var resp *httptest.ResponseRecorder
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		resp = f.post(context.Background(), "/api/v1/jobs/job_shop/run", "")
		if resp.Code != http.StatusConflict || time.Now().After(deadline) {
			break
		}
	}
	acceptedID(t, resp)
	f.waitStarted(t)
}

func TestShutdownCancelsBackgroundBackup(t *testing.T) {
	f := newAsyncFixture(t)
	id := acceptedID(t, f.post(context.Background(), "/api/v1/backups", `{"connection_id":"conn_test","database":"shop"}`))
	f.waitStarted(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.runs.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	rec, err := f.store.GetBackupRecord(context.Background(), id)
	if err != nil || rec.Status != models.StatusFailed || !strings.Contains(rec.ErrorMessage, "cancel") {
		t.Fatalf("cancelled backup must be persisted as failed: %+v, %v", rec, err)
	}
	if resp := f.post(context.Background(), "/api/v1/backups", `{"connection_id":"conn_test","database":"shop"}`); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("new run after shutdown: expected 503, got %d", resp.Code)
	}
}

func TestRestoreOfEncryptedBackupWithoutKeyIsRejectedSynchronously(t *testing.T) {
	f := newAsyncFixture(t)
	src := &models.BackupRecord{ID: "bkp_enc", Database: "shop", ConnectionID: testConnID, Status: models.StatusCompleted, StorageKey: "shop/x.archive.age", Encrypted: true}
	if err := f.store.SaveBackupRecord(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	resp := f.post(context.Background(), "/api/v1/restore", `{"backup_id":"bkp_enc"}`)
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", resp.Code, resp.Body.String())
	}
	if restores, _ := f.store.ListRestoreRecords(context.Background()); len(restores) != 0 {
		t.Fatalf("no restore record may be created, got %d", len(restores))
	}
}

func TestStatsCountsCompletedBackupsAndEnabledJobs(t *testing.T) {
	f := newAsyncFixture(t)
	ctx := context.Background()
	for _, b := range []*models.BackupRecord{
		{ID: "b1", Database: "d", Status: models.StatusCompleted, SizeBytes: 100},
		{ID: "b2", Database: "d", Status: models.StatusCompleted, SizeBytes: 50},
		{ID: "b3", Database: "d", Status: models.StatusFailed},
		{ID: "b4", Database: "d", Status: models.StatusInProgress},
	} {
		if err := f.store.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range []*models.Job{
		{ID: "j1", Database: "d", CronExpression: "@daily", Enabled: true},
		{ID: "j2", Database: "d", CronExpression: "@daily", Enabled: false},
		{ID: "j3", Database: "d", CronExpression: "@daily", Enabled: true},
	} {
		if err := f.store.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/stats", nil))
	var res struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"total_backups":     4,
		"completed_backups": 2,
		"failed_backups":    1,
		"total_bytes":       150,
		"active_jobs":       2,
	}
	for k, v := range want {
		if res.Data[k] != v {
			t.Errorf("stats[%s] = %v, want %v", k, res.Data[k], v)
		}
	}
}
