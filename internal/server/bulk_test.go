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
		{"both selections", "/api/v1/backups/bulk", `{"action":"delete","ids":["a"],"filter":{},"dry_run":true}`, http.StatusBadRequest, "either ids or filter"},
		{"foreign filter", "/api/v1/jobs/bulk", `{"action":"delete","filter":{"status":"failed"},"dry_run":true}`, http.StatusBadRequest, "do not apply"},
		{"filter on a real delete", "/api/v1/backups/bulk", `{"action":"delete","filter":{"status":"failed"},"confirm_count":3}`, http.StatusBadRequest, "actionable_ids"},
		{"bad time", "/api/v1/restores/bulk", `{"action":"delete","filter":{"from":"monday"},"dry_run":true}`, http.StatusBadRequest, "RFC 3339"},
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
	run, _ := json.Marshal(map[string]any{"action": "delete", "ids": dry.Data.ActionableIDs, "confirm_count": 12})
	if rec = serve(f.h, "POST", "/api/v1/backups/bulk", run, h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"succeeded":12`) {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	list, _ := f.store.ListBackupRecords(ctx, "")
	for _, b := range list {
		if b.Status != models.StatusDeleted {
			t.Fatalf("%s = %s; want deleted (soft, waiting for its purge)", b.ID, b.Status)
		}
	}
	if len(list) != 12 {
		t.Fatalf("%d backup records; want the 12 deleted ones kept until their purge", len(list))
	}

	rec = serve(f.h, "GET", "/api/v1/bulk/actions", nil, map[string]string{"X-API-Key": f.keys[auth.ScopeRead]})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resource":"jobs","name":"run_now","scope":"operator","destructive":false,"allowed":false`) {
		t.Fatalf("actions for a read key: %d %s", rec.Code, rec.Body)
	}
}

// TestSingleDeleteIsSoftOverHTTP checks DELETE /api/v1/backups/{id}: the answer
// carries the purge time, the archive stays, the list hides the backup unless asked
// for deleted ones, a second delete is refused, a restore is refused and the
// undelete endpoint brings it back.
func TestSingleDeleteIsSoftOverHTTP(t *testing.T) {
	srv, st, mock := setupTestServer(t)
	ctx := context.Background()
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "sd_1", Database: "shop", Status: models.StatusCompleted,
		StorageKey: "shop/sd_1.archive.gz", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mock.Save(ctx, "shop/sd_1.archive.gz", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	h := asPrincipal(srv.mux, auth.SystemPrincipal())
	rec := serve(h, "DELETE", "/api/v1/backups/sd_1?reason=cleanup", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"deleted"`) || !strings.Contains(rec.Body.String(), `"purge_after":"`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := mock.Stat(ctx, "shop/sd_1.archive.gz"); err != nil {
		t.Fatal("a delete removed the archive before the grace period")
	}
	if rec = serve(h, "DELETE", "/api/v1/backups/sd_1", nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete again: %d %s; want 409", rec.Code, rec.Body)
	}
	if rec = serve(h, "GET", "/api/v1/backups", nil, nil); strings.Contains(rec.Body.String(), "sd_1") {
		t.Fatalf("default list shows a deleted backup: %s", rec.Body)
	}
	for _, q := range []string{"?deleted=true", "?status=deleted"} {
		if rec = serve(h, "GET", "/api/v1/backups"+q, nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sd_1") {
			t.Fatalf("list %s: %d %s; want the deleted backup", q, rec.Code, rec.Body)
		}
	}
	if rec = serve(h, "GET", "/api/v1/backups?deleted=true&status=completed", nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("deleted with another status: %d; want 400", rec.Code)
	}
	body := []byte(`{"backup_id":"sd_1","target_connection_id":"conn_x"}`)
	if rec = serve(h, "POST", "/api/v1/restore", body, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "undelete") {
		t.Fatalf("restore of a deleted backup: %d %s; want 409", rec.Code, rec.Body)
	}
	if rec = serve(h, "POST", "/api/v1/backups/sd_1/undelete", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"completed"`) {
		t.Fatalf("undelete: %d %s", rec.Code, rec.Body)
	}
	if rec = serve(h, "POST", "/api/v1/backups/sd_1/undelete", nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("undelete again: %d %s; want 409", rec.Code, rec.Body)
	}
}
