package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/cli"
)

// TestAccessRoutesAreRegistered keeps the access matrix's tables honest: every
// route they name is registered.
func TestAccessRoutesAreRegistered(t *testing.T) {
	f := newAccessFixture(t)
	registered := f.authenticatedPatterns()
	for _, table := range []map[string]bool{keys(accessPathRoutes), keys(accessBodyRoutes), keys(accessBulkRoutes)} {
		for p := range table {
			if !slices.Contains(registered, p) {
				t.Errorf("%s is in an access table but not registered", p)
			}
		}
	}
	for _, p := range append(slices.Clone(accessUnrelatedIDRoutes), accessLimitedNotFound...) {
		if !slices.Contains(registered, p) {
			t.Errorf("%s is in an access list but not registered", p)
		}
	}
}

func keys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// get serves GET path with headers and returns the status and the body.
func (f *accessFixture) get(path string, headers map[string]string) (int, string) {
	rec := serve(f.h, http.MethodGet, path, nil, headers)
	return rec.Code, rec.Body.String()
}

// adminKey creates an unlimited admin key.
func (f *accessFixture) adminKey(t *testing.T) map[string]string {
	t.Helper()
	_, plain, err := f.auth.CreateAPIKey(context.Background(), auth.SystemPrincipal(), "admin", auth.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Authorization": "Bearer " + plain, "Content-Type": "application/json"}
}

func TestListsShowOnlyTheCallersConnections(t *testing.T) {
	f := newAccessFixture(t)
	admin := f.adminKey(t)
	for _, tc := range []struct {
		path string
		a, b string
	}{
		{"/api/v1/connections", testConnID, accessConnB},
		{"/api/v1/storage-targets", accessTgtA, accessTgtB},
		{"/api/v1/jobs", accessJobA, accessJobB},
		{"/api/v1/backups", accessBkpA, accessBkpB},
		{"/api/v1/backups/databases", "shop", accessDBB},
		{"/api/v1/restores", accessRstA, accessRstB},
		{"/api/v1/restores/databases", "shop_rescue_1", accessDBB},
		{"/api/v1/readiness", testConnID, accessConnB},
		{"/api/v1/stats", accessJobA, accessJobB},
		{"/api/v1/pitr/streams", accessPstA, accessPstB},
	} {
		for _, h := range []map[string]string{f.keyHeaders(), f.session(t)} {
			code, body := f.get(tc.path, h)
			if code != http.StatusOK || !strings.Contains(body, tc.a) || strings.Contains(body, tc.b) {
				t.Errorf("limited GET %s: %d %s; want %s and not %s", tc.path, code, body, tc.a, tc.b)
			}
		}
		// An unlimited caller sees both sides.
		if code, body := f.get(tc.path, admin); code != http.StatusOK || !strings.Contains(body, tc.a) || !strings.Contains(body, tc.b) {
			t.Errorf("admin GET %s: %d %s; want both sides", tc.path, code, body)
		}
	}
	// A filter naming B finds nothing, exactly like an unknown connection.
	for _, path := range []string{"/api/v1/backups?connection_id=" + accessConnB, "/api/v1/backups?job_id=" + accessJobB,
		"/api/v1/restores?backup_id=" + accessBkpB, "/api/v1/backups?id=" + accessBkpB} {
		code, body := f.get(path, f.keyHeaders())
		var res struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &res); code != http.StatusOK || err != nil || len(res.Data) != 0 {
			t.Errorf("limited GET %s: %d %s; want an empty list", path, code, body)
		}
	}
}

func TestRestoreIntoAnotherConnectionIsNotFound(t *testing.T) {
	f := newAccessFixture(t)
	for _, path := range []string{"/api/v1/restore", "/api/v1/restores/preflight"} {
		body := `{"backup_id":"` + accessBkpA + `","target_connection_id":"` + accessConnB + `"}`
		rec := serve(f.h, http.MethodPost, path, []byte(body), f.keyHeaders())
		if !hiddenNotFound(rec.Code, rec.Body.String()) {
			t.Errorf("POST %s into B: %d %s; want 404", path, rec.Code, rec.Body.String())
		}
		// An unknown connection answers the same.
		body = `{"backup_id":"` + accessBkpA + `","target_connection_id":"conn_nope"}`
		if rec = serve(f.h, http.MethodPost, path, []byte(body), f.keyHeaders()); !hiddenNotFound(rec.Code, rec.Body.String()) {
			t.Errorf("POST %s into an unknown connection: %d %s; want the same 404", path, rec.Code, rec.Body.String())
		}
	}
	// Bulk filters select only A's records.
	rec := serve(f.h, http.MethodPost, "/api/v1/backups/bulk", []byte(`{"action":"pin","filter":{},"dry_run":true}`), f.keyHeaders())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), accessBkpA) || strings.Contains(rec.Body.String(), accessBkpB) {
		t.Errorf("bulk pin by filter: %d %s; want only A's backup", rec.Code, rec.Body.String())
	}
}

func TestUserConnectionsOverHTTP(t *testing.T) {
	f := newAccessFixture(t)
	admin := f.adminKey(t)
	// /auth/me reports the limit; an unlimited caller has none.
	for _, h := range []map[string]string{f.keyHeaders(), f.session(t)} {
		if code, body := f.get("/api/v1/auth/me", h); code != http.StatusOK || !strings.Contains(body, `"connection_ids":["`+testConnID+`"]`) {
			t.Errorf("me of a limited caller: %d %s", code, body)
		}
	}
	if _, body := f.get("/api/v1/auth/me", admin); !strings.Contains(body, `"all_connections":true`) {
		t.Errorf("me of an unlimited caller: %s; want all_connections", body)
	}
	if _, body := f.get("/api/v1/users", admin); !strings.Contains(body, `"connection_ids":["`+testConnID+`"]`) {
		t.Errorf("users: %s; want the operator's connections", body)
	}

	put := func(id, body string) (int, string) {
		rec := serve(f.h, http.MethodPut, "/api/v1/users/"+id+"/connections", []byte(body), admin)
		return rec.Code, rec.Body.String()
	}
	if code, body := put(f.operator.ID, `{"connection_ids":["`+testConnID+`","`+accessConnB+`"]}`); code != http.StatusOK ||
		!strings.Contains(body, accessConnB) {
		t.Fatalf("widen: %d %s", code, body)
	}
	if code, body := f.get("/api/v1/backups/"+accessBkpB+"/collections", f.session(t)); hiddenNotFound(code, body) {
		t.Errorf("after widening the user: %d %s; want B readable", code, body)
	}
	if code, body := put(f.operator.ID, `{"connection_ids":["conn_nope"]}`); code != http.StatusBadRequest {
		t.Errorf("unknown connection: %d %s; want 400", code, body)
	}
	if code, body := put(f.operator.ID, `{"all_connections":true,"connection_ids":["`+testConnID+`"]}`); code != http.StatusBadRequest {
		t.Errorf("all_connections with a list: %d %s; want 400", code, body)
	}
	if code, body := put(f.operator.ID, `{}`); code != http.StatusBadRequest {
		t.Errorf("neither field: %d %s; want 400", code, body)
	}

	// An empty list is none, never every connection.
	if code, body := put(f.operator.ID, `{"connection_ids":[]}`); code != http.StatusOK || !strings.Contains(body, `"all_connections":false`) {
		t.Fatalf("limit to none: %d %s", code, body)
	}
	if code, body := f.get("/api/v1/connections", f.session(t)); code != http.StatusOK || strings.Contains(body, testConnID) {
		t.Errorf("a user with no connection lists: %d %s; want none", code, body)
	}
	// A deleted connection still on the list is dropped, not a reason to widen:
	// limited to A and a connection deleted since, the admin saves A alone.
	if code, body := put(f.operator.ID, `{"connection_ids":["`+testConnID+`"]}`); code != http.StatusOK {
		t.Fatalf("limit to A: %d %s", code, body)
	}
	if err := f.st.UpdateUserConnections(context.Background(), "", f.operator.ID,
		auth.ConnectionAccess{ConnectionIDs: []string{testConnID, "conn_deleted"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, body := put(f.operator.ID, `{"connection_ids":["`+testConnID+`","conn_deleted"]}`); code != http.StatusOK ||
		strings.Contains(body, "conn_deleted") || !strings.Contains(body, `"connection_ids":["`+testConnID+`"]`) {
		t.Errorf("saving a list with a deleted connection: %d %s; want it dropped", code, body)
	}
	if err := f.st.UpdateUserConnections(context.Background(), "", f.operator.ID,
		auth.ConnectionAccess{ConnectionIDs: []string{"conn_deleted"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, body := put(f.operator.ID, `{"connection_ids":["conn_deleted"]}`); code != http.StatusOK || !strings.Contains(body, `"connection_ids":[]`) {
		t.Errorf("a list of only a deleted connection: %d %s; want none, not every connection", code, body)
	}
	users, err := f.auth.ListUsers(context.Background(), auth.SystemPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	for _, u := range users {
		if u.Role == auth.RoleAdmin {
			adminID = u.ID
		}
	}
	if code, body := put(adminID, `{"connection_ids":["`+testConnID+`"]}`); code != http.StatusBadRequest || !strings.Contains(body, "administrators") {
		t.Errorf("limit an admin: %d %s; want 400", code, body)
	}
	// A limited operator cannot change anyone's connections, not even their own.
	rec := serve(f.h, http.MethodPut, "/api/v1/users/"+f.operator.ID+"/connections", []byte(`{"connection_ids":[]}`), f.session(t))
	if rec.Code != http.StatusForbidden {
		t.Errorf("operator lifts their own limit: %d %s; want 403", rec.Code, rec.Body.String())
	}
	// An admin key cannot be limited.
	rec = serve(f.h, http.MethodPost, "/api/v1/api-keys", []byte(`{"name":"x","scope":"admin","connection_ids":["`+testConnID+`"]}`), admin)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limited admin key: %d %s; want 400", rec.Code, rec.Body.String())
	}
}

func TestLimitedKeysAreCappedByTheirCreator(t *testing.T) {
	f := newAccessFixture(t)
	// The limited operator's own key, created without a limit, inherits theirs.
	rec := serve(f.h, http.MethodPost, "/api/v1/api-keys", []byte(`{"name":"mine","scope":"operator","all_connections":true}`), f.session(t))
	var res struct {
		Data struct {
			Key    string `json:"key"`
			APIKey struct {
				Effective []string `json:"effective_connection_ids"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != http.StatusCreated || err != nil ||
		!slices.Equal(res.Data.APIKey.Effective, []string{testConnID}) {
		t.Fatalf("key of a limited user: %d %s", rec.Code, rec.Body.String())
	}
	h := map[string]string{"Authorization": "Bearer " + res.Data.Key}
	if code, body := f.get("/api/v1/connections/"+accessConnB, h); !hiddenNotFound(code, body) {
		t.Errorf("inherited key reads B: %d %s", code, body)
	}
	// The key list shows the effective connections.
	if code, body := f.get("/api/v1/api-keys", f.session(t)); code != http.StatusOK || !strings.Contains(body, `"effective_connection_ids":["`+testConnID+`"]`) {
		t.Errorf("api keys: %d %s", code, body)
	}
}

func TestCLIFollowsConnectionAccess(t *testing.T) {
	f := newAccessFixture(t)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	run := func(args ...string) (int, string) {
		app := &cli.App{Version: "test", PollInterval: time.Millisecond, HTTPClient: ts.Client()}
		var stdout, stderr bytes.Buffer
		env := map[string]string{cli.EnvAPIKey: f.key, cli.EnvURL: ts.URL}
		code := app.Run(context.Background(), args, func(k string) string { return env[k] }, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	for _, list := range []string{"backups", "restores", "jobs", "connections", "targets"} {
		code, out := run("list", list, "--json")
		if code != cli.ExitOK || leaked(out, "") != "" {
			t.Errorf("list %s: %d %s; want no record of B", list, code, out)
		}
	}
	for _, args := range [][]string{
		{"verify", accessBkpB},
		{"restore", accessBkpB},
		{"backup", "--job", accessJobB},
		{"backup", "--connection", accessConnB, "--database", accessDBB},
	} {
		if code, out := run(args...); code != cli.ExitNotFound {
			t.Errorf("%v: exit %d %s; want %d (not found)", args, code, out, cli.ExitNotFound)
		}
	}
	if code, out := run("list", "backups", "--json"); code != cli.ExitOK || !strings.Contains(out, accessBkpA) {
		t.Errorf("list backups: %d %s; want A's backup", code, out)
	}
}
