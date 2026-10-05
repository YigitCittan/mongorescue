package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestMovedJobsShowOnlyTheCallersBackups checks that a backup's own connection
// decides what a limited caller sees, not its job's: job A once ran on connection B.
func TestMovedJobsShowOnlyTheCallersBackups(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	later := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	done := later.Add(time.Minute)
	if err := f.st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_moved", JobID: accessJobA, Database: "shop", ConnectionID: accessConnB,
		StorageTargetID: accessTgtB, StorageType: models.StorageLocal, StorageKey: "shop/bkp_moved.archive", Status: models.StatusCompleted,
		StartedAt: later, CompletedAt: &done, SizeBytes: 9, Verification: models.VerificationOK}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/stats", "/api/v1/stats/history", "/api/v1/readiness", "/api/v1/backups", "/api/v1/jobs/" + accessJobA} {
		code, body := f.get(path, f.keyHeaders())
		if code != http.StatusOK || strings.Contains(body, "bkp_moved") {
			t.Errorf("limited GET %s: %d %s; want no backup of B", path, code, body)
		}
	}
	code, body := f.get("/api/v1/stats", f.keyHeaders())
	if code != http.StatusOK || !strings.Contains(body, `"`+accessJobA+`":{"id":"`+accessBkpA+`"`) {
		t.Errorf("limited stats: %d %s; want job A's last backup from A", code, body)
	}
	if code, body = f.get("/api/v1/readiness", f.keyHeaders()); code != http.StatusOK || !strings.Contains(body, `"last_good_backup":{"id":"`+accessBkpA+`"`) {
		t.Errorf("limited readiness: %d %s; want A's backup as the last good one", code, body)
	}
	if code, body = f.get("/api/v1/backups/bkp_moved/collections", f.keyHeaders()); !hiddenNotFound(code, body) {
		t.Errorf("limited GET of B's backup of job A: %d %s; want 404", code, body)
	}
	// An unlimited caller sees it.
	if _, body = f.get("/api/v1/stats", f.adminKey(t)); !strings.Contains(body, "bkp_moved") {
		t.Errorf("admin stats: %s; want the newest backup", body)
	}
}
