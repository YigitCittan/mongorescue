package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/vault"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// fakeKV answers kv-v2 reads of secret/mongo and the database engine.
func fakeKV(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/secret/data/mongo":
			_, _ = w.Write([]byte(`{"data":{"data":{"username":"kv-user-7","password":"kv-pass-7"},"metadata":{"version":2}}}`))
		case "/v1/database/creds/backup":
			_, _ = w.Write([]byte(`{"lease_id":"database/creds/backup/l1","lease_duration":3600,"renewable":true,"data":{"username":"v-dyn","password":"dyn-pass-7"}}`))
		case "/v1/sys/leases/revoke":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVaultEndpointsNeverReturnSecrets(t *testing.T) {
	vs := fakeKV(t)
	client, err := vault.New(vault.Config{Addr: vs.URL, AllowInsecure: true, HTTPClient: vs.Client(), Auth: vault.Auth{Method: vault.AuthToken, Token: "s.t"}})
	if err != nil {
		t.Fatal(err)
	}
	resolver := vaultcreds.New(vaultcreds.Config{Client: client})
	t.Cleanup(resolver.Close)
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(newTestAuth(t, st, testAPIKey)), WithVaultCredentials(resolver))
	h := keyed{full.Handler()}
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		return serve(h, method, path, raw, nil)
	}

	if rec := do("GET", "/api/v1/vault", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"configured":true`) {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []map[string]any{
		{"use": "connection", "vault": map[string]any{"engine": "kv-v2", "path": "mongo"}},
		{"use": "connection", "vault": map[string]any{"engine": "database", "role": "backup"}},
	} {
		rec := do("POST", "/api/v1/vault/test", body)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("test %v = %d %s", body, rec.Code, rec.Body.String())
		}
		for _, secret := range []string{"kv-pass-7", "kv-user-7", "dyn-pass-7", "v-dyn"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("the test answer holds %q: %s", secret, rec.Body.String())
			}
		}
	}
	if rec := do("POST", "/api/v1/vault/test", map[string]any{"use": "connection", "vault": map[string]any{"engine": "aws", "role": "x"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid reference = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/api/v1/vault/test", map[string]any{"use": "elsewhere"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid use = %d", rec.Code)
	}
	rec := do("POST", "/api/v1/vault/test", map[string]any{"use": "connection", "vault": map[string]any{"engine": "kv-v2", "path": "absent"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("missing secret = %d %s", rec.Code, rec.Body.String())
	}
}
