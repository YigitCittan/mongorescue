package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// connFixture is a keyed full server with a scriptable prober.
type connFixture struct {
	h      http.Handler
	store  *store.SQLiteStore
	prober *fakeProber
}

func newConnFixture(t *testing.T) *connFixture {
	t.Helper()
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	p := &fakeProber{}
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(newTestAuth(t, st, testAPIKey)), withTestConnection(t, st, p))
	return &connFixture{h: keyed{full.Handler()}, store: st, prober: p}
}

func (f *connFixture) do(method, path string, body any) *httptest.ResponseRecorder {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return serve(f.h, method, path, raw, nil)
}

func TestConnectionsAPINeverServesCredentials(t *testing.T) {
	f := newConnFixture(t)
	for _, rec := range []*httptest.ResponseRecorder{
		f.do("GET", "/api/v1/connections", nil),
		f.do("GET", "/api/v1/connections/"+testConnID, nil),
		f.do("PUT", "/api/v1/connections/"+testConnID, map[string]string{"name": "renamed", "uri": redact.URI(testConnURI)}),
	} {
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "s3cret-pw") || !strings.Contains(rec.Body.String(), "admin:"+redact.Mask+"@") {
			t.Fatalf("response %d leaks or lacks the mask: %s", rec.Code, rec.Body.String())
		}
	}
	// The redacted round trip kept the stored password.
	if c, _ := f.store.GetConnection(context.Background(), testConnID); c.URI != testConnURI || c.Name != "renamed" {
		t.Fatalf("stored connection = %+v", c)
	}
}

func TestConnectionsCRUD(t *testing.T) {
	f := newConnFixture(t)
	const uri = "mongodb://ops:hunter2-hunter2@replica.example.net:27017/?replicaSet=rs0"

	for _, bad := range []map[string]string{
		{"name": "", "uri": uri},
		{"name": "x", "uri": "postgres://u:p@h/db"},
		{"name": "x", "uri": "mongodb://u:pa/ss@h/db"},
		{"name": "x", "uri": "mongodb://u:" + redact.Mask + "@h/db"},
	} {
		rec := f.do("POST", "/api/v1/connections", bad)
		if rec.Code != http.StatusBadRequest || (bad["uri"] != "" && strings.Contains(rec.Body.String(), bad["uri"])) {
			t.Fatalf("create %v: %d %s; want 400 without echo", bad, rec.Code, rec.Body.String())
		}
	}

	rec := f.do("POST", "/api/v1/connections", map[string]string{"name": "replica", "uri": uri, "description": "prod"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var c models.Connection
	decodeData(t, rec, &c)
	if strings.Contains(c.URI, "hunter2") || c.Description != "prod" {
		t.Fatalf("created = %+v", c)
	}

	// A masked URI for a different host must not inherit the password.
	other := strings.Replace(c.URI, "replica.example.net", "attacker.example.com", 1)
	if rec := f.do("PUT", "/api/v1/connections/"+c.ID, map[string]string{"name": "replica", "uri": other}); rec.Code != http.StatusBadRequest {
		t.Fatalf("masked uri for another host: %d; want 400", rec.Code)
	}
	if rec := f.do("PUT", "/api/v1/connections/conn_missing", map[string]string{"name": "x", "uri": uri}); rec.Code != http.StatusNotFound {
		t.Fatalf("update missing: %d", rec.Code)
	}

	// Delete: 409 while a job uses it, then 200, then 404.
	job := models.Job{ID: "job_uses_conn", Database: "shop", CronExpression: "@daily", ConnectionID: c.ID}
	if rec := f.do("POST", "/api/v1/jobs", job); rec.Code != http.StatusCreated {
		t.Fatalf("create job: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do("DELETE", "/api/v1/connections/"+c.ID, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "used by jobs") {
		t.Fatalf("delete in use: %d %s; want 409", rec.Code, rec.Body.String())
	}
	if rec := f.do("DELETE", "/api/v1/jobs/job_uses_conn", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete job: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/api/v1/connections/"+c.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := f.do("DELETE", "/api/v1/connections/"+c.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", rec.Code)
	}
}

func TestConnectionTestEndpoints(t *testing.T) {
	f := newConnFixture(t)

	var res connections.TestResult
	decodeData(t, f.do("POST", "/api/v1/connections/"+testConnID+"/test", nil), &res)
	if !res.OK || res.ServerVersion != "7.0.14" || f.prober.uri() != testConnURI {
		t.Fatalf("saved test = %+v (prober must get the stored URI)", res)
	}
	var c models.Connection
	decodeData(t, f.do("GET", "/api/v1/connections/"+testConnID, nil), &c)
	if !c.LastTestOK || c.LastTestAt == nil || c.ServerVersion != "7.0.14" {
		t.Fatalf("test result not recorded: %+v", c)
	}

	// Failures are reported with HTTP 200 and a redacted error.
	f.prober.mu.Lock()
	f.prober.pingErr = errUnreachable
	f.prober.mu.Unlock()
	rec := f.do("POST", "/api/v1/connections/"+testConnID+"/test", nil)
	decodeData(t, rec, &res)
	if rec.Code != http.StatusOK || res.OK || res.Error == "" || strings.Contains(rec.Body.String(), "s3cret-pw") {
		t.Fatalf("failed test: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do("POST", "/api/v1/connections/test", map[string]string{"uri": "mongodb://new:pw-new-123@h:27017/"})
	decodeData(t, rec, &res)
	if rec.Code != http.StatusOK || res.OK {
		t.Fatalf("unsaved failing test: %d %+v", rec.Code, res)
	}

	f.prober.mu.Lock()
	f.prober.pingErr = nil
	f.prober.mu.Unlock()
	decodeData(t, f.do("POST", "/api/v1/connections/test", map[string]string{"uri": "mongodb://new:pw-new-123@h:27017/"}), &res)
	if !res.OK || f.prober.uri() != "mongodb://new:pw-new-123@h:27017/" {
		t.Fatalf("unsaved test = %+v", res)
	}
	for _, body := range []map[string]string{{"uri": "nope://"}, {"uri": redact.URI(testConnURI)}, {"uri": ""}} {
		if rec := f.do("POST", "/api/v1/connections/test", body); rec.Code != http.StatusBadRequest {
			t.Errorf("test %v: %d; want 400", body, rec.Code)
		}
	}
	if rec := f.do("POST", "/api/v1/connections/conn_missing/test", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("test missing: %d", rec.Code)
	}
}

func TestConnectionTestIsBoundedByTimeout(t *testing.T) {
	f := newConnFixture(t)
	f.prober.mu.Lock()
	f.prober.delay = time.Minute // longer than the fixture's 2s test timeout
	f.prober.mu.Unlock()
	start := time.Now()
	var res connections.TestResult
	decodeData(t, f.do("POST", "/api/v1/connections/"+testConnID+"/test", nil), &res)
	if res.OK || time.Since(start) > 10*time.Second {
		t.Fatalf("slow server test = %+v after %v", res, time.Since(start))
	}
}

func TestConnectionDiscoveryEndpoints(t *testing.T) {
	f := newConnFixture(t)
	f.prober.mu.Lock()
	f.prober.dbs = []connections.Database{{Name: "shop", SizeBytes: 42}, {Name: "admin"}, {Name: "local"}}
	f.prober.cols = []connections.Collection{{Name: "orders", Type: "collection"}}
	f.prober.mu.Unlock()

	var dbs []connections.Database
	decodeData(t, f.do("GET", "/api/v1/connections/"+testConnID+"/databases", nil), &dbs)
	if len(dbs) != 1 || dbs[0].Name != "shop" || dbs[0].SizeBytes != 42 {
		t.Fatalf("databases = %+v", dbs)
	}
	decodeData(t, f.do("GET", "/api/v1/connections/"+testConnID+"/databases?system=true", nil), &dbs)
	if len(dbs) != 3 {
		t.Fatalf("databases with system = %+v", dbs)
	}
	var cols []connections.Collection
	decodeData(t, f.do("GET", "/api/v1/connections/"+testConnID+"/databases/shop/collections", nil), &cols)
	if len(cols) != 1 || cols[0].Type != "collection" {
		t.Fatalf("collections = %+v", cols)
	}

	f.prober.mu.Lock()
	f.prober.pingErr = errUnreachable
	f.prober.mu.Unlock()
	rec := f.do("GET", "/api/v1/connections/"+testConnID+"/databases", nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "s3cret-pw") {
		t.Fatalf("unreachable server: %d %s; want 502 without credentials", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", "/api/v1/connections/conn_missing/databases", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing connection: %d", rec.Code)
	}
}

func TestOperationsRequireAConnection(t *testing.T) {
	f := newConnFixture(t)
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/api/v1/jobs", models.Job{Database: "shop", CronExpression: "@daily"}},
		{"/api/v1/jobs", models.Job{Database: "shop", CronExpression: "@daily", ConnectionID: "conn_missing"}},
		{"/api/v1/backups", models.BackupOptions{Database: "shop"}},
		{"/api/v1/backups", models.BackupOptions{Database: "shop", ConnectionID: "conn_missing"}},
	} {
		if rec := f.do("POST", tc.path, tc.body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "connection_id") {
			t.Errorf("POST %s %+v: %d %s; want 400", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
	// The legacy mongo_uri field is no longer accepted: it is simply ignored.
	rec := f.do("POST", "/api/v1/backups", map[string]string{"database": "shop", "mongo_uri": "mongodb://evil:pw@attacker/"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mongo_uri without connection_id: %d", rec.Code)
	}
}

func TestBackupAndCrossServerRestoreRecordConnections(t *testing.T) {
	f := newConnFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.store.SaveConnection(ctx, &models.Connection{ID: "conn_dr", Name: "dr site", URI: "mongodb://dr:pw-dr-site@dr.internal/", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	bkpID := acceptedID(t, f.do("POST", "/api/v1/backups", models.BackupOptions{Database: "shop", ConnectionID: testConnID, ExcludeCollections: []string{"logs"}}))
	got := awaitRecord(t, f.h, "/api/v1/backups", bkpID)
	if got["connection_id"] != testConnID || got["connection_name"] != "test server" || got["status"] != "completed" {
		t.Fatalf("backup record = %v", got)
	}

	if rec := f.do("POST", "/api/v1/restore", models.RestoreRequest{BackupID: bkpID, TargetConnectionID: "conn_missing"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("restore into unknown connection: %d", rec.Code)
	}
	rstID := acceptedID(t, f.do("POST", "/api/v1/restore", models.RestoreRequest{BackupID: bkpID, TargetConnectionID: "conn_dr"}))
	rst := awaitRecord(t, f.h, "/api/v1/restores", rstID)
	if rst["target_connection_id"] != "conn_dr" || rst["target_connection_name"] != "dr site" || rst["status"] != "completed" {
		t.Fatalf("cross-server restore record = %v", rst)
	}
	rstID = acceptedID(t, f.do("POST", "/api/v1/restore", models.RestoreRequest{BackupID: bkpID}))
	if rst := awaitRecord(t, f.h, "/api/v1/restores", rstID); rst["target_connection_id"] != testConnID {
		t.Fatalf("default restore target = %v; want the backup's connection", rst["target_connection_id"])
	}
}
