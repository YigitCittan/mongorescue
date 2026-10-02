package store

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestRestoreVerificationIsMirrored checks that a restore's verification round-trips
// and is mirrored into the column added by migration 0016 (NULL without one).
func TestRestoreVerificationIsMirrored(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	verified := &models.RestoreRecord{ID: "rst_verified", BackupID: "bkp", SourceDatabase: "shop", TargetDatabase: "shop_rescue",
		Status: models.RestoreStatusCompleted, StartedAt: at,
		Verification: &models.RestoreVerification{Status: models.RestoreVerificationPassed, Collections: 3, CheckedAt: at}}
	plain := &models.RestoreRecord{ID: "rst_plain", BackupID: "bkp", SourceDatabase: "shop", TargetDatabase: "shop_rescue2",
		Status: models.RestoreStatusCompleted, StartedAt: at}
	for _, r := range []*models.RestoreRecord{verified, plain} {
		if err = s.SaveRestoreRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetRestoreRecord(ctx, "rst_verified")
	if err != nil || got.Verification == nil || got.Verification.Status != models.RestoreVerificationPassed || got.Verification.Collections != 3 {
		t.Fatalf("verified = %+v, %v", got, err)
	}
	var status string
	if err = s.db.QueryRowContext(ctx, "SELECT json_extract(verification, '$.status') FROM restores WHERE id = 'rst_verified'").Scan(&status); err != nil || status != "passed" {
		t.Fatalf("verification column = %q, %v", status, err)
	}
	var raw sql.NullString
	if err = s.db.QueryRowContext(ctx, "SELECT verification FROM restores WHERE id = 'rst_plain'").Scan(&raw); err != nil || raw.Valid {
		t.Fatalf("verification column without a verification = %+v, %v; want NULL", raw, err)
	}
}
