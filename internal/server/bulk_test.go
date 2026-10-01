package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// bulkRoutes are the bulk endpoints and an action each accepts.
var bulkRoutes = []struct{ path, action string }{
	{"/api/v1/backups/bulk", "delete"},
	{"/api/v1/restores/bulk", "delete"},
	{"/api/v1/jobs/bulk", "delete"},
}

func TestBulkRoutesNeedScopeAndCSRF(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := f.auth.Setup(context.Background(), "192.0.2.1", f.auth.SetupCode(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	for _, r := range bulkRoutes {
		body := []byte(`{"action":"` + r.action + `","ids":["x"],"dry_run":true}`)
		with := func(h map[string]string) map[string]string {
			h["Content-Type"] = "application/json"
			return h
		}
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{})); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d", r.path, rec.Code)
		}
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})); !scopeRefused(rec.Code, rec.Body.String()) {
			t.Errorf("%s read key: %d %s; want the scope refusal", r.path, rec.Code, rec.Body)
		}
		// Deleting needs admin, like the single-item routes, even for a dry run.
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{"X-API-Key": f.keys[auth.ScopeOperator]})); rec.Code != http.StatusForbidden {
			t.Errorf("%s operator delete: %d %s; want 403", r.path, rec.Code, rec.Body)
		}
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin]})); rec.Code != http.StatusOK {
			t.Errorf("%s admin dry run: %d %s; want 200", r.path, rec.Code, rec.Body)
		}
		cookie, csrf := newSession(t, f)
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{"Cookie": cookie})); rec.Code != http.StatusForbidden {
			t.Errorf("%s session without CSRF: %d; want 403", r.path, rec.Code)
		}
		if rec := serve(f.h, "POST", r.path, body, with(map[string]string{"Cookie": cookie, CSRFHeader: csrf})); rec.Code != http.StatusOK {
			t.Errorf("%s session with CSRF: %d %s; want 200", r.path, rec.Code, rec.Body)
		}
	}
}

func TestBulkValidationErrors(t *testing.T) {
	f := newScopeFixture(t)
	h := map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin], "Content-Type": "application/json"}
	many := make([]string, 0, 10001)
	for i := range 10001 {
		many = append(many, fmt.Sprintf("b%d", i))
	}
	manyJSON, _ := json.Marshal(map[string]any{"action": "delete", "ids": many, "dry_run": true})
	for _, c := range []struct {
		name, path, body string
		code             int
		msg              string
	}{
		{"not json", "/api/v1/backups/bulk", `nope`, http.StatusBadRequest, "invalid request json"},
		{"empty body", "/api/v1/backups/bulk", ``, http.StatusBadRequest, "empty body"},
		{"two objects", "/api/v1/backups/bulk", `{"action":"delete","ids":["a"]}{}`, http.StatusBadRequest, "one JSON object"},
		{"misspelt filter", "/api/v1/backups/bulk", `{"action":"delete","filter":{"stauts":"failed"}}`, http.StatusBadRequest, `unknown field \"stauts\"`},
		{"unknown top-level field", "/api/v1/jobs/bulk", `{"action":"delete","ids":["a"],"force":true}`, http.StatusBadRequest, "unknown field"},
		{"no action", "/api/v1/backups/bulk", `{"ids":["a"]}`, http.StatusBadRequest, "unknown action"},
		{"unknown action", "/api/v1/restores/bulk", `{"action":"verify","ids":["a"]}`, http.StatusBadRequest, "unknown action"},
		{"no selection", "/api/v1/backups/bulk", `{"action":"delete"}`, http.StatusBadRequest, "either ids or filter"},
		{"both selections", "/api/v1/backups/bulk", `{"action":"delete","ids":["a"],"filter":{}}`, http.StatusBadRequest, "either ids or filter"},
		{"foreign filter", "/api/v1/jobs/bulk", `{"action":"delete","filter":{"status":"failed"}}`, http.StatusBadRequest, "do not apply"},
		{"bad time", "/api/v1/restores/bulk", `{"action":"delete","filter":{"from":"monday"}}`, http.StatusBadRequest, "RFC 3339"},
		{"too many", "/api/v1/backups/bulk", string(manyJSON), http.StatusUnprocessableEntity, "at most 10000"},
		{"confirm mismatch", "/api/v1/backups/bulk", `{"action":"delete","ids":["a"],"confirm_count":5}`, http.StatusConflict, "confirm_count is 5"},
	} {
		rec := serve(f.h, "POST", c.path, []byte(c.body), h)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: %d %s; want %d mentioning %q", c.name, rec.Code, rec.Body, c.code, c.msg)
		}
	}
}

func TestBulkDeleteOverHTTP(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 12 {
		id := fmt.Sprintf("bk_%02d", i)
		if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusFailed, StartedAt: now.Add(-time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	h := map[string]string{"X-API-Key": f.keys[auth.ScopeAdmin], "Content-Type": "application/json"}
	filter := []byte(`{"action":"delete","filter":{"status":"failed","database":"shop"},"dry_run":true}`)
	rec := serve(f.h, "POST", "/api/v1/backups/bulk", filter, h)
	var dry struct {
		Data struct {
			Matched       int      `json:"matched"`
			Actionable    int      `json:"actionable"`
			ActionableIDs []string `json:"actionable_ids"`
			Skipped       []any    `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dry); err != nil || rec.Code != http.StatusOK || dry.Data.Matched != 12 || dry.Data.Actionable != 12 || dry.Data.Skipped == nil {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	run := []byte(`{"action":"delete","filter":{"status":"failed","database":"shop"},"confirm_count":12}`)
	if rec = serve(f.h, "POST", "/api/v1/backups/bulk", run, h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"succeeded":12`) {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if list, _ := f.store.ListBackupRecords(ctx, ""); len(list) != 0 {
		t.Fatalf("%d backups left", len(list))
	}

	rec = serve(f.h, "GET", "/api/v1/bulk/actions", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resource":"jobs","name":"run_now","scope":"operator","destructive":false,"allowed":false`) {
		t.Fatalf("actions for a read key: %d %s", rec.Code, rec.Body)
	}
}

func TestSingleDeleteKeepsSharedArchives(t *testing.T) {
	srv, st, mock := setupTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"sh_1", "sh_2"} {
		if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/shared.archive.gz"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mock.Save(ctx, "shop/shared.archive.gz", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	h := asPrincipal(srv.mux, auth.SystemPrincipal())
	rec := serve(h, "DELETE", "/api/v1/backups/sh_1", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "archive_kept") {
		t.Fatalf("delete sharer: %d %s", rec.Code, rec.Body)
	}
	if _, err := mock.Stat(ctx, "shop/shared.archive.gz"); err != nil {
		t.Fatal("the shared archive was deleted")
	}
	if rec = serve(h, "DELETE", "/api/v1/backups/sh_2", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"archive_deleted":true`) {
		t.Fatalf("delete last sharer: %d %s", rec.Code, rec.Body)
	}
	if rec = serve(h, "DELETE", "/api/v1/backups/sh_2", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d %s", rec.Code, rec.Body)
	}
}
