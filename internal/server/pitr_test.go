package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// pitrFixture serves the stream API with an inspector whose answer the test sets.
type pitrFixture struct {
	h       http.Handler
	keys    map[auth.Scope]string
	inspect collector.Inspection
	inspErr error
	enc     *encryption.Encryptor
	bases   int
}

func newPITRFixture(t *testing.T) *pitrFixture {
	t.Helper()
	base, _, _ := setupTestServer(t)
	st := storetest.New(t)
	svc := newTestAuth(t, st, "")
	_, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	f := &pitrFixture{keys: map[auth.Scope]string{}, enc: enc,
		inspect: collector.Inspection{Window: pitr.OplogWindow{ReplicaSet: "rs0"}, CanReadOplog: true}}
	col := collector.New(collector.Config{
		Repo: st,
		Open: func(context.Context, *pitr.Stream) (collector.Session, error) {
			return nil, io.ErrUnexpectedEOF
		},
		Storage:   func(context.Context, string) (storage.Storage, error) { return storage.NewMockStorage(), nil },
		Encryptor: func() *encryption.Encryptor { return f.enc },
		Inspect: func(context.Context, string) (collector.Inspection, error) {
			return f.inspect, f.inspErr
		},
		StartBase: func(_ context.Context, id string, _ models.BackupTrigger) (*models.BackupRecord, error) {
			f.bases++
			return &models.BackupRecord{ID: "bkp_pitr-base_x", PITRStreamID: id, Scope: models.ScopeInstance}, nil
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := NewServer(bootConfig(), st, base.backupEngine, base.restoreEngine, base.storageDriver, base.scheduler, nil, nil,
		WithAuth(svc), withTestConnection(t, st, nil), WithSettings(newTestSettings(t, st, newTestConfig().Security)),
		WithMetricsHandler(ok), WithMCPHandler(ok), WithAudit(audit.NewService(st, nil)), withTestOIDC(t), WithPITR(col))
	f.h = srv.Handler()
	for _, scope := range auth.Scopes() {
		_, plain, err := svc.CreateAPIKey(context.Background(), auth.SystemPrincipal(), string(scope)+" key", scope)
		if err != nil {
			t.Fatal(err)
		}
		f.keys[scope] = plain
	}
	return f
}

func (f *pitrFixture) do(scope auth.Scope, method, path, body string) (int, string) {
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	rec := serve(f.h, method, path, b, map[string]string{"Authorization": "Bearer " + f.keys[scope], "Content-Type": "application/json"})
	return rec.Code, rec.Body.String()
}

func TestPITRStreamAPI(t *testing.T) {
	f := newPITRFixture(t)

	// Validation: a replica set, oplog access and encryption are required.
	f.inspErr = pitr.ErrNotReplicaSet
	if code, body := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a","enabled":false}`); code != http.StatusBadRequest || !strings.Contains(body, "replica set") {
		t.Fatalf("standalone: %d %s", code, body)
	}
	f.inspErr = nil
	f.inspect.CanReadOplog = false
	if code, body := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a"}`); code != http.StatusBadRequest || !strings.Contains(body, "oplog") {
		t.Fatalf("no oplog access: %d %s", code, body)
	}
	f.inspect.CanReadOplog = true
	enc := f.enc
	f.enc = nil
	if code, body := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a"}`); code != http.StatusBadRequest || !strings.Contains(body, "encryption") {
		t.Fatalf("no encryption: %d %s", code, body)
	}
	f.enc = enc
	if code, _ := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a","chunk_seconds":5}`); code != http.StatusBadRequest {
		t.Fatalf("chunk_seconds 5: %d", code)
	}
	if code, _ := f.do(auth.ScopeOperator, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a"}`); code != http.StatusForbidden {
		t.Fatalf("operator creates a stream: %d", code)
	}

	code, body := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a","enabled":false}`)
	if code != http.StatusCreated || !strings.Contains(body, `"replica_set":"rs0"`) || !strings.Contains(body, `"chunk_seconds":60`) {
		t.Fatalf("create: %d %s", code, body)
	}
	id := between(body, `"id":"`, `"`)
	if code, _ := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", `{"connection_id":"conn_a"}`); code != http.StatusConflict {
		t.Fatalf("second stream of a connection: %d", code)
	}
	if code, body = f.do(auth.ScopeRead, "GET", "/api/v1/pitr/streams/"+id, ""); code != http.StatusOK || !strings.Contains(body, `"experimental":true`) {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body = f.do(auth.ScopeRead, "GET", "/api/v1/pitr/streams", ""); code != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body = f.do(auth.ScopeRead, "GET", "/api/v1/pitr/streams/"+id+"/chunks?limit=10", ""); code != http.StatusOK || !strings.Contains(body, `"total":0`) {
		t.Fatalf("chunks: %d %s", code, body)
	}
	if code, _ = f.do(auth.ScopeRead, "GET", "/api/v1/pitr/streams/"+id+"/chunks?limit=x", ""); code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", code)
	}
	if code, _ = f.do(auth.ScopeAdmin, "PATCH", "/api/v1/pitr/streams/"+id, `{"connection_id":"conn_b"}`); code != http.StatusBadRequest {
		t.Fatalf("moving a stream: %d", code)
	}
	if code, body = f.do(auth.ScopeAdmin, "PATCH", "/api/v1/pitr/streams/"+id, `{"enabled":true,"chunk_seconds":120}`); code != http.StatusOK || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("enable: %d %s", code, body)
	}
	if code, body = f.do(auth.ScopeOperator, "POST", "/api/v1/pitr/streams/"+id+"/base", ""); code != http.StatusAccepted || f.bases != 1 {
		t.Fatalf("base: %d %s", code, body)
	}
	if code, _ = f.do(auth.ScopeAdmin, "DELETE", "/api/v1/pitr/streams/"+id, ""); code != http.StatusConflict {
		t.Fatalf("deleting an enabled stream: %d", code)
	}
	if code, _ = f.do(auth.ScopeAdmin, "PATCH", "/api/v1/pitr/streams/"+id, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if code, body = f.do(auth.ScopeAdmin, "DELETE", "/api/v1/pitr/streams/"+id, ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ = f.do(auth.ScopeRead, "GET", "/api/v1/pitr/streams/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("deleted stream: %d", code)
	}
}

// between returns the text of s between the first after and the next before.
func between(s, after, before string) string {
	_, rest, ok := strings.Cut(s, after)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, before)
	return v
}
