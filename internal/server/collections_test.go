package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

func TestBackupCollectionsEndpoint(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	archive, err := os.ReadFile(filepath.Join("..", "mongotools", "testdata", "prelude", "shop.archive.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.srv.storageDriver.Save(ctx, "shop/2026/09/bkp_shop_prev.archive.gz", bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []*models.BackupRecord{
		{ID: "bkp_shop_prev", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/2026/09/bkp_shop_prev.archive.gz"},
		{ID: "bkp_shop_enc", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/2026/09/bkp_shop_enc.archive.gz.age", Encrypted: true},
		{ID: "bkp_shop_old", Database: "shop", Status: models.StatusCompleted, StorageKey: "shop/2026/09/gone.archive", Collections: []string{"orders"}},
	} {
		if err := f.store.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	get := func(scope auth.Scope, id string) (int, apiResponse, string) {
		rec := serve(f.h, "GET", "/api/v1/backups/"+id+"/collections", nil, map[string]string{"X-API-Key": f.keys[scope]})
		var res apiResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		return rec.Code, res, rec.Body.String()
	}

	// Every scope may read the list (read is enough).
	for _, scope := range auth.Scopes() {
		code, res, body := get(scope, "bkp_shop_prev")
		if code != http.StatusOK || !res.Success {
			t.Fatalf("%s key: %d %s", scope, code, body)
		}
		var list operations.BackupCollections
		raw, _ := json.Marshal(res.Data)
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if list.Source != operations.CollectionsFromArchive || len(list.Collections) != 4 || list.Collections[3].ViewOn != "orders" {
			t.Fatalf("list = %+v", list)
		}
	}
	if code, _, body := get(auth.ScopeRead, "bkp_missing"); code != http.StatusNotFound {
		t.Fatalf("missing backup = %d %s; want 404", code, body)
	}
	if code, _, body := get(auth.ScopeRead, "bkp_shop_enc"); code != http.StatusUnprocessableEntity || !strings.Contains(body, restore.KeyRequiredHint) {
		t.Fatalf("encrypted backup without a key = %d %s; want 422 with the key hint", code, body)
	}
	code, res, body := get(auth.ScopeRead, "bkp_shop_old")
	if code != http.StatusOK || !strings.Contains(body, `"source":"record"`) || !strings.Contains(body, `"name":"orders"`) || !res.Success {
		t.Fatalf("unreadable archive = %d %s; want the record's list", code, body)
	}
	// No credentials, no list.
	if rec := serve(f.h, "GET", "/api/v1/backups/bkp_shop_prev/collections", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request = %d; want 401", rec.Code)
	}
}
