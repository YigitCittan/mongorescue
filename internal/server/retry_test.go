package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestRetryBackupEndpoint(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	failedAt := time.Now().UTC().Add(-time.Minute)
	for _, r := range []*models.BackupRecord{
		{ID: "bkp_failed", Trigger: models.TriggerManual, Database: "shop", ConnectionID: testConnID, Status: models.StatusFailed,
			StorageType: models.StorageLocal, StartedAt: failedAt.Add(-time.Minute), CompletedAt: &failedAt,
			ErrorMessage: "start mongodump: mongodump not found", Collections: []string{"orders"}},
		{ID: "bkp_done", Database: "shop", ConnectionID: testConnID, Status: models.StatusCompleted, StartedAt: failedAt},
		{ID: "bkp_orphan", Database: "shop", ConnectionID: "conn_deleted", Status: models.StatusFailed, StartedAt: failedAt},
	} {
		if err := f.store.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	as := func(scope auth.Scope) map[string]string {
		return map[string]string{"X-API-Key": f.keys[scope], "Content-Type": "application/json"}
	}

	// The read scope may not retry; operator may, like starting a backup.
	if rec := serve(f.h, "POST", "/api/v1/backups/bkp_failed/retry", nil, as(auth.ScopeRead)); !scopeRefused(rec.Code, rec.Body.String()) {
		t.Fatalf("read key retrying = %d %s; want a scope refusal", rec.Code, rec.Body.String())
	}
	rec := serve(f.h, "POST", "/api/v1/backups/bkp_failed/retry", nil, as(auth.ScopeOperator))
	id := acceptedID(t, rec)
	var res struct {
		Data models.BackupRecord `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Data.RetryOf != "bkp_failed" || res.Data.Database != "shop" || res.Data.ConnectionID != testConnID ||
		res.Data.Trigger != models.TriggerManual || len(res.Data.Collections) != 1 || res.Data.Collections[0] != "orders" {
		t.Fatalf("retry response = %+v; want a copy of the failed backup linked by retry_of", res.Data)
	}
	if !strings.Contains(rec.Body.String(), `"retry_of":"bkp_failed"`) {
		t.Fatalf("retry_of not exposed: %s", rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := f.store.GetBackupRecord(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != models.StatusInProgress {
			if got.Status != models.StatusCompleted || got.RetryOf != "bkp_failed" {
				t.Fatalf("retry outcome = %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if orig, err := f.store.GetBackupRecord(ctx, "bkp_failed"); err != nil || orig.Status != models.StatusFailed ||
		orig.ErrorMessage == "" || orig.CompletedAt == nil {
		t.Fatalf("original after retry = %+v, %v; want it kept as failed", orig, err)
	}

	for _, tc := range []struct {
		path string
		want int
		msg  string
	}{
		{"/api/v1/backups/bkp_done/retry", http.StatusConflict, "only failed backups can be retried"},
		{"/api/v1/backups/bkp_missing/retry", http.StatusNotFound, "backup not found"},
		{"/api/v1/backups/bkp_orphan/retry", http.StatusUnprocessableEntity, "no longer exists"},
	} {
		if rec := serve(f.h, "POST", tc.path, nil, as(auth.ScopeOperator)); rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("POST %s = %d %s; want %d mentioning %q", tc.path, rec.Code, rec.Body.String(), tc.want, tc.msg)
		}
	}
}
