package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestDeleteKeepsSharedArchives(t *testing.T) {
	h, st, mem, _ := integrityServer(t)
	ctx := context.Background()
	rec := seedArchive(t, st, mem, "bkp_a", []byte("archive"))
	twin := *rec
	twin.ID, twin.Status = "bkp_twin", models.StatusPruned
	if err := st.SaveBackupRecord(ctx, &twin); err != nil {
		t.Fatal(err)
	}

	// Another (pruned) record names the archive: only the record goes.
	code, data := call(t, h, http.MethodDelete, "/api/v1/backups/bkp_a", "")
	if code != http.StatusOK || !strings.Contains(string(data), `"archive_deleted":false`) || !strings.Contains(string(data), "bkp_twin") {
		t.Fatalf("delete shared = %d %s", code, data)
	}
	if _, err := mem.Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("a shared archive must stay: %v", err)
	}
	// The last record that names it takes the archive with it.
	if code, data = call(t, h, http.MethodDelete, "/api/v1/backups/bkp_twin", ""); code != http.StatusOK || !strings.Contains(string(data), `"archive_deleted":true`) {
		t.Fatalf("delete last = %d %s", code, data)
	}
	if _, err := mem.Stat(ctx, rec.StorageKey); err == nil {
		t.Fatal("the archive of the last record must be deleted")
	}

	// A pinned record that shares the archive keeps it on legal hold.
	rec = seedArchive(t, st, mem, "bkp_b", []byte("archive b"))
	pinned := *rec
	pinned.ID, pinned.Pinned = "bkp_b_pinned", true
	if err := st.SaveBackupRecord(ctx, &pinned); err != nil {
		t.Fatal(err)
	}
	code, data = call(t, h, http.MethodDelete, "/api/v1/backups/bkp_b", "")
	if code != http.StatusOK || !strings.Contains(string(data), "pinned") || !strings.Contains(string(data), "bkp_b_pinned") {
		t.Fatalf("delete next to a pinned twin = %d %s", code, data)
	}
	if _, err := mem.Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("an archive a pinned record names must stay: %v", err)
	}
}

func TestUnpinNeedsAdmin(t *testing.T) {
	if requiredScope("POST /api/v1/backups/{id}/unpin") != "admin" || requiredScope("POST /api/v1/backups/{id}/pin") != "operator" {
		t.Fatal("pinning is operator, lifting a legal hold is admin")
	}
}
