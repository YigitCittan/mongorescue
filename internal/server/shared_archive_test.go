package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestDeletedArchivesAreNeitherOrphansNorImportable checks that a deleted backup's
// archive, kept for the grace period, is not reported as an orphan by a storage scan
// and cannot be imported as a new backup; a purged record's leftover archive is an
// orphan again.
func TestDeletedArchivesAreNeitherOrphansNorImportable(t *testing.T) {
	h, st, mem, manager := integrityServer(t)
	ctx := context.Background()
	rec := seedArchive(t, st, mem, "bkp_shop_20260901_030000_aaaa", []byte("archive"))
	code, data := call(t, h, http.MethodDelete, "/api/v1/backups/"+rec.ID, "")
	if code != http.StatusOK || !strings.Contains(string(data), `"archive_deleted":false`) {
		t.Fatalf("delete = %d %s", code, data)
	}
	if _, err := mem.Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("a deleted backup's archive must stay: %v", err)
	}
	code, data = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/scan", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"orphan_count":0`) || !strings.Contains(string(data), `"missing_count":0`) {
		t.Fatalf("scan with a deleted backup = %d %s; want no orphan and nothing missing", code, data)
	}
	code, data = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/import", `{"key":"`+rec.StorageKey+`"}`)
	if code != http.StatusConflict || !strings.Contains(string(data), rec.ID) {
		t.Fatalf("import of a deleted backup's archive = %d %s; want 409", code, data)
	}
	if got, _ := st.GetBackupRecord(ctx, rec.ID); got.Status != models.StatusDeleted {
		t.Fatalf("record = %s; want still deleted", got.Status)
	}

	// Once purged (here with its archive left behind), the object is an orphan again.
	if _, err := st.UpdateBackupRecord(ctx, rec.ID, func(r *models.BackupRecord) error { r.Status = models.StatusPurged; return nil }); err != nil {
		t.Fatal(err)
	}
	code, data = call(t, h, http.MethodPost, "/api/v1/storage-targets/tgt_local/scan", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"orphan_count":1`) {
		t.Fatalf("scan with a purged record = %d %s; want one orphan", code, data)
	}
	waitRuns(t, manager)
}

func TestUnpinNeedsAdmin(t *testing.T) {
	if requiredScope("POST /api/v1/backups/{id}/unpin") != "admin" || requiredScope("POST /api/v1/backups/{id}/pin") != "operator" {
		t.Fatal("pinning is operator, lifting a legal hold is admin")
	}
}
