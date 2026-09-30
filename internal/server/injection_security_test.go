package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestClientsCannotChooseTheStorageKey checks that POST /api/v1/backups ignores a
// target_key: an operator key could otherwise overwrite another backup's archive (a
// delete it is not allowed to make) or aim at paths outside the storage root.
func TestClientsCannotChooseTheStorageKey(t *testing.T) {
	h, st, mock := newChainServerWithStorage(t, nil)
	victim := &models.BackupRecord{ID: "bkp_victim", Database: "shop", ConnectionID: testConnID, Status: models.StatusCompleted,
		StorageKey: "shop/2026/01/bkp_victim.archive.gz"}
	if err := st.SaveBackupRecord(context.Background(), victim); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{victim.StorageKey, "../../../../tmp/owned", "/etc/cron.d/x"} {
		body, _ := json.Marshal(map[string]any{"connection_id": testConnID, "database": "shop", "target_key": key})
		rec := serve(h, "POST", "/api/v1/backups", body, map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("backup with target_key %q: %d %s", key, rec.Code, rec.Body)
		}
		var started models.BackupRecord
		decodeData(t, rec, &started)
		if started.StorageKey == key || !strings.HasPrefix(started.StorageKey, "shop/") || strings.Contains(started.StorageKey, "..") {
			t.Fatalf("target_key %q was honoured: storage key %q", key, started.StorageKey)
		}
		awaitRecord(t, h, "/api/v1/backups", started.ID)
	}
	for _, key := range []string{victim.StorageKey, "../../../../tmp/owned", "/etc/cron.d/x"} {
		if _, err := mock.Stat(context.Background(), key); err == nil {
			t.Fatalf("an archive was written to the client-chosen key %q", key)
		}
	}
}

// TestHostileNamespacesAreRefusedByTheAPI checks that backups, jobs and in-place
// restores naming databases or collections MongoDB cannot have (newlines, NUL, path
// separators, spaces before flags) are refused with 400 before anything runs.
func TestHostileNamespacesAreRefusedByTheAPI(t *testing.T) {
	h, st := newChainServer(t, nil)
	ctx := context.Background()
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_src", Database: "shop", ConnectionID: testConnID,
		Status: models.StatusCompleted, StorageKey: "shop/src.archive"}); err != nil {
		t.Fatal(err)
	}
	// A job saved before validation existed, with a name the API now refuses.
	if err := st.SaveJob(ctx, &models.Job{ID: "job_existing", Name: "legacy", Database: "legacy db", ConnectionID: testConnID, CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"backup, newline", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop\n--drop"}},
		{"backup, NUL", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop\x00"}},
		{"backup, traversal", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "../../etc"}},
		{"backup, flag after space", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop --drop"}},
		{"backup, leading dash", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "--drop"}},
		{"job update, leading dash", "PUT", "/api/v1/jobs/job_existing", map[string]any{"connection_id": testConnID, "database": "-h", "cron_expression": "@daily"}},
		{"backup, collection newline", "POST", "/api/v1/backups", map[string]any{"connection_id": testConnID, "database": "shop", "collections": []string{"a\nb"}}},
		{"job, dot", "POST", "/api/v1/jobs", map[string]any{"connection_id": testConnID, "database": "shop.orders", "cron_expression": "@daily"}},
		{"job, excluded collection NUL", "POST", "/api/v1/jobs", map[string]any{"connection_id": testConnID, "database": "shop", "cron_expression": "@daily", "exclude_collections": []string{"a\x00"}}},
		{"in-place restore, newline", "POST", "/api/v1/restore", map[string]any{"backup_id": "bkp_src", "safe_clone": false, "confirm_in_place": true, "verify": false, "target_database": "prod\n--drop"}},
		{"restore, dollar collection", "POST", "/api/v1/restore", map[string]any{"backup_id": "bkp_src", "selected_collections": []string{"$cmd"}}},
		{"in-place restore, leading dash", "POST", "/api/v1/restore", map[string]any{"backup_id": "bkp_src", "safe_clone": false, "confirm_in_place": true, "verify": false, "target_database": "--drop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			rec := serve(h, tc.method, tc.path, body, map[string]string{"Content-Type": "application/json"})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid namespace") {
				t.Fatalf("%d %s; want 400 invalid namespace", rec.Code, rec.Body)
			}
		})
	}
	if list, _ := st.ListBackupRecords(ctx, ""); len(list) != 1 {
		t.Fatalf("refused backups were recorded: %d records", len(list))
	}
	if list, _ := st.ListRestoreRecords(ctx); len(list) != 0 {
		t.Fatalf("refused restores were recorded: %+v", list)
	}
	if jobs, _ := st.ListJobs(ctx); len(jobs) != 1 || jobs[0].Database != "legacy db" {
		t.Fatalf("refused jobs were saved or changed: %+v", jobs)
	}
}
