package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// runFixture is a server whose scheduler shares its store, as in the application,
// so the backups of a run it starts are listed.
type runFixture struct{ h http.Handler }

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("backup-data"))), strings.NewReader("ok"), func() error { return nil }, nil
	}
	bEngine := backup.NewEngine(mock, "", backup.WithRunner(runner))
	sched := scheduler.NewScheduler(st, bEngine, mock, nil)
	srv := NewServer(bootConfig(), st, bEngine, restore.NewEngine(mock, ""), mock, sched, nil, nil, withTestConnection(t, st, nil))
	return &runFixture{h: srv.buildRoutes()}
}

func (f *runFixture) do(method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return serve(f.h, method, path, raw, nil)
}

func TestCreateBackupOfSeveralDatabases(t *testing.T) {
	f := newRunFixture(t)

	rec := f.do("POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "databases": []string{"shop", "crm"}, "parallelism": 2})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST databases: %d %s; want 202", rec.Code, rec.Body.String())
	}
	var run operations.BackupRun
	decodeData(t, rec, &run)
	if run.RunID == "" || len(run.Backups) != 2 || len(run.Busy) != 0 {
		t.Fatalf("run = %+v; want a run ID, two backups and none busy", run)
	}
	for i, want := range []string{"shop", "crm"} {
		b := run.Backups[i]
		if b.Database != want || b.RunID != run.RunID || b.ConnectionID != testConnID || b.JobID != "" {
			t.Errorf("backup %d = %+v; want %s in run %s", i, b, want, run.RunID)
		}
		if got := awaitRecord(t, f.h, "/api/v1/backups", b.ID); got["status"] != "completed" {
			t.Errorf("backup of %s = %v; want completed", want, got)
		}
	}
	// The Backups list finds the run's backups by run_id.
	list := f.do("GET", "/api/v1/backups?run_id="+run.RunID, nil)
	var page []map[string]any
	decodeData(t, list, &page)
	if len(page) != 2 {
		t.Fatalf("backups of run %s = %d; want 2", run.RunID, len(page))
	}

	for name, body := range map[string]map[string]any{
		"database and databases":      {"connection_id": testConnID, "database": "shop", "databases": []string{"crm"}},
		"no databases":                {"connection_id": testConnID, "databases": []string{}},
		"duplicate":                   {"connection_id": testConnID, "databases": []string{"shop", "shop"}},
		"invalid name":                {"connection_id": testConnID, "databases": []string{"shop", "a.b"}},
		"collections with several":    {"connection_id": testConnID, "databases": []string{"shop", "crm"}, "collections": []string{"orders"}},
		"parallelism":                 {"connection_id": testConnID, "databases": []string{"shop", "crm"}, "parallelism": 9},
		"no connection":               {"databases": []string{"shop"}},
		"users and roles of admin db": {"connection_id": testConnID, "databases": []string{"admin"}, "include_users_and_roles": true},
	} {
		if rec := f.do("POST", "/api/v1/backups", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", name, rec.Code, rec.Body.String())
		}
	}
}

// TestCreateBackupSingleUnchanged checks that a request with database answers the
// backup record itself, as before databases existed.
func TestCreateBackupSingleUnchanged(t *testing.T) {
	f := newRunFixture(t)
	rec := f.do("POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop"})
	body := rec.Body.String()
	id := acceptedID(t, rec)
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run_id", "backups", "busy"} {
		if _, ok := env.Data[key]; ok {
			t.Errorf("single backup response has %q: %s", key, body)
		}
	}
	if !strings.HasPrefix(id, "bkp_") {
		t.Errorf("single backup id = %q", id)
	}
	awaitRecord(t, f.h, "/api/v1/backups", id)
}
