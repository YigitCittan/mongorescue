package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// runServerFixture serves the full middleware chain with a run registry and backups
// that block in a fake mongodump until they are cancelled.
type runServerFixture struct {
	*scopeFixture
	registry *runs.Registry
}

func newRunServerFixture(t *testing.T) *runServerFixture {
	t.Helper()
	st := storetest.New(t)
	svc := newTestAuth(t, st, "")
	blocking := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, strings.NewReader("2026-10-01T10:00:00.000+0000\twriting shop.orders to archive on stdout\n" +
				"connecting to mongodb://admin:s3cret-pw@db.internal:27017\n"),
			func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
	mock := storage.NewMockStorage()
	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	srv := NewServer(bootConfig(), st, backup.NewEngine(mock, "", backup.WithRunner(blocking)), restore.NewEngine(mock, ""), mock, nil, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithRunManager(manager), WithRunRegistry(reg))
	f := &runServerFixture{scopeFixture: &scopeFixture{srv: srv, h: srv.Handler(), keys: map[auth.Scope]string{}, store: st, auth: svc}, registry: reg}
	for _, scope := range auth.Scopes() {
		_, plain, err := svc.CreateAPIKey(context.Background(), auth.SystemPrincipal(), string(scope)+" key", scope)
		if err != nil {
			t.Fatal(err)
		}
		f.keys[scope] = plain
	}
	return f
}

func (f *runServerFixture) as(scope auth.Scope) map[string]string {
	return map[string]string{"X-API-Key": f.keys[scope], "Content-Type": "application/json"}
}

func TestCancelBackupEndpoint(t *testing.T) {
	f := newRunServerFixture(t)
	body := []byte(`{"connection_id":"` + testConnID + `","database":"shop"}`)
	id := acceptedID(t, serve(f.h, "POST", "/api/v1/backups", body, f.as(auth.ScopeOperator)))
	for deadline := time.Now().Add(5 * time.Second); f.registry.Get(id) == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the backup never became active")
		}
	}

	// Live progress: the active runs endpoint and the list item.
	rec := serve(f.h, "GET", "/api/v1/runs/active", nil, f.as(auth.ScopeRead))
	var active struct {
		Data []models.RunProgress `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil || rec.Code != http.StatusOK || len(active.Data) != 1 ||
		active.Data[0].ID != id || active.Data[0].Kind != models.RunBackup {
		t.Fatalf("active runs = %d %s", rec.Code, rec.Body.String())
	}
	if res := serve(f.h, "GET", "/api/v1/backups", nil, f.as(auth.ScopeRead)); !strings.Contains(res.Body.String(), `"progress":{"id":"`+id+`"`) {
		t.Fatalf("the running backup has no progress in the list: %s", res.Body.String())
	}

	// Following the log while it runs.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		rec = serve(f.h, "GET", "/api/v1/backups/"+id+"/log?tail=20", nil, f.as(auth.ScopeRead))
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "writing shop.orders") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live log = %d %q", rec.Code, rec.Body.String())
		}
	}

	if res := serve(f.h, "POST", "/api/v1/backups/"+id+"/cancel", nil, f.as(auth.ScopeRead)); !scopeRefused(res.Code, res.Body.String()) {
		t.Fatalf("read key cancelling = %d %s; want a scope refusal", res.Code, res.Body.String())
	}
	cancelled := serve(f.h, "POST", "/api/v1/backups/"+id+"/cancel", nil, f.as(auth.ScopeOperator))
	var answer struct {
		Data models.BackupRecord `json:"data"`
	}
	if err := json.Unmarshal(cancelled.Body.Bytes(), &answer); err != nil || cancelled.Code != http.StatusOK ||
		answer.Data.ID != id || answer.Data.Status != models.StatusCancelled || answer.Data.CancelledBy != "API key operator key" {
		t.Fatalf("cancel = %d %s; want 200 with the final cancelled record", cancelled.Code, cancelled.Body.String())
	}
	var final *models.BackupRecord
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		b, err := f.store.GetBackupRecord(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != models.StatusInProgress && f.registry.Get(id) == nil {
			final = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled backup did not stop")
		}
	}
	if final.Status != models.StatusCancelled || final.CancelledBy != "API key operator key" {
		t.Fatalf("final = %+v", final)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v1/backups/" + id + "/cancel", http.StatusConflict},
		{"/api/v1/backups/bkp_unknown/cancel", http.StatusNotFound},
		{"/api/v1/restores/rst_unknown/cancel", http.StatusNotFound},
	} {
		if res := serve(f.h, "POST", tc.path, nil, f.as(auth.ScopeOperator)); res.Code != tc.want {
			t.Errorf("POST %s = %d %s; want %d", tc.path, res.Code, res.Body.String(), tc.want)
		}
	}
	if res := serve(f.h, "GET", "/api/v1/runs/active", nil, f.as(auth.ScopeRead)); !strings.Contains(res.Body.String(), `"data":[]`) {
		t.Fatalf("active runs after the cancel = %s", res.Body.String())
	}

	// The finished log: tail and download, redacted.
	rec = serve(f.h, "GET", "/api/v1/backups/"+id+"/log?tail=3", nil, f.as(auth.ScopeRead))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		rec.Header().Get("Content-Disposition") != "" || strings.Count(rec.Body.String(), "\n") != 3 ||
		!strings.Contains(rec.Body.String(), "backup cancelled by API key operator key") {
		t.Fatalf("tail = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = serve(f.h, "GET", "/api/v1/backups/"+id+"/log", nil, f.as(auth.ScopeRead))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != `attachment; filename="`+id+`.log"` ||
		rec.Header().Get("Content-Length") == "" || !strings.Contains(rec.Body.String(), "[mongorescue] backup "+id+" of database shop started") {
		t.Fatalf("download = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret-pw") || !strings.Contains(rec.Body.String(), "admin:******@db.internal") {
		t.Fatalf("the downloaded log is not redacted: %q", rec.Body.String())
	}
	for _, q := range []string{"?tail=0", "?tail=-1", "?tail=abc", "?tail=100000"} {
		if rec := serve(f.h, "GET", "/api/v1/backups/"+id+"/log"+q, nil, f.as(auth.ScopeRead)); rec.Code != http.StatusBadRequest {
			t.Errorf("tail %s = %d", q, rec.Code)
		}
	}
	for _, path := range []string{"/api/v1/backups/bkp_unknown/log", "/api/v1/restores/rst_unknown/log"} {
		if rec := serve(f.h, "GET", path, nil, f.as(auth.ScopeRead)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}

	// Deleting the backup deletes its log.
	if rec := serve(f.h, "DELETE", "/api/v1/backups/"+id, nil, f.as(auth.ScopeAdmin)); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.registry.Logs().Open(id); !errors.Is(err, runlog.ErrNotFound) {
		t.Fatalf("log after the backup was deleted: %v", err)
	}
}

func TestRestoreLogWithoutFileIs404(t *testing.T) {
	f := newRunServerFixture(t)
	r := &models.RestoreRecord{ID: "rst_old", TargetDatabase: "shop_rescue", Status: models.RestoreStatusCompleted, StartedAt: time.Now()}
	if err := f.store.SaveRestoreRecord(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	rec := serve(f.h, "GET", "/api/v1/restores/rst_old/log", nil, f.as(auth.ScopeRead))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no log was recorded") {
		t.Fatalf("log of a restore from an older release = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(f.h, "POST", "/api/v1/restores/rst_old/cancel", nil, f.as(auth.ScopeOperator)); rec.Code != http.StatusConflict {
		t.Fatalf("cancelling a finished restore = %d %s", rec.Code, rec.Body.String())
	}
}

func TestLogFileName(t *testing.T) {
	for id, want := range map[string]string{
		"bkp_shop_20260101_000000_ab12cd34": "bkp_shop_20260101_000000_ab12cd34.log",
		`bkp_a"b;c`:                         "bkp_a_b_c.log",
		"":                                  "run.log",
	} {
		if got := logFileName(id); got != want {
			t.Errorf("logFileName(%q) = %q, want %q", id, got, want)
		}
	}
}
