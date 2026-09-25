package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestFailInterruptedRuns(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "running", Database: "d", Status: models.StatusInProgress, SHA256: "e3b0"})
	_ = fs.SaveBackupRecord(ctx, &models.BackupRecord{ID: "done", Database: "d", Status: models.StatusCompleted, SHA256: "abcd"})
	_ = fs.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rst", Status: models.RestoreStatusInProgress})

	a := &App{metaStore: fs, logger: slog.Default()}
	a.failInterruptedRuns(ctx)

	if b, _ := fs.GetBackupRecord(ctx, "running"); b.Status != models.StatusFailed || b.SHA256 != "" || b.ErrorMessage == "" {
		t.Fatalf("interrupted backup not failed: %+v", b)
	}
	if b, _ := fs.GetBackupRecord(ctx, "done"); b.Status != models.StatusCompleted || b.SHA256 != "abcd" {
		t.Fatalf("completed backup modified: %+v", b)
	}
	restores, _ := fs.ListRestoreRecords(ctx)
	if len(restores) != 1 || restores[0].Status != models.RestoreStatusFailed {
		t.Fatalf("interrupted restore not failed: %+v", restores)
	}
}
