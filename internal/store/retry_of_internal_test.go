package store

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestRetryOfIsStoredAndIndexed checks that a retry's retry_of round-trips and is
// mirrored into the indexed column added by migration 0009.
func TestRetryOfIsStoredAndIndexed(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, r := range []*models.BackupRecord{
		{ID: "bkp_failed", Database: "shop", Status: models.StatusFailed},
		{ID: "bkp_retry", Database: "shop", Status: models.StatusCompleted, RetryOf: "bkp_failed"},
	} {
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetBackupRecord(ctx, "bkp_retry")
	if err != nil || got.RetryOf != "bkp_failed" {
		t.Fatalf("retry = %+v, %v; want retry_of bkp_failed", got, err)
	}
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM backups WHERE retry_of = ?", "bkp_failed").Scan(&id); err != nil || id != "bkp_retry" {
		t.Fatalf("retry_of column lookup = %q, %v", id, err)
	}
}
