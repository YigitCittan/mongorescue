package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

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

func TestFailInterruptedJobRuns(t *testing.T) {
	ctx := context.Background()
	fs := storetest.New(t)
	run := &models.JobRun{ID: "run_1", JobID: "job", Status: models.JobRunRunning, StartedAt: time.Now().Add(-time.Hour),
		Databases: []models.JobRunDatabase{
			{Database: "a", BackupID: "bkp_a", Status: models.StatusCompleted},
			{Database: "b", BackupID: "bkp_b", Status: models.StatusInProgress},
		}}
	if err := fs.SaveJobRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	done := &models.JobRun{ID: "run_0", JobID: "job", Status: models.JobRunOK, StartedAt: time.Now().Add(-2 * time.Hour)}
	if err := fs.SaveJobRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	a := &App{metaStore: fs, logger: slog.Default()}
	a.failInterruptedRuns(ctx)

	got, err := fs.GetJobRun(ctx, "run_1")
	if err != nil || got.Status != models.JobRunPartial || got.Databases[1].Status != models.StatusFailed || got.CompletedAt == nil {
		t.Fatalf("interrupted run = %+v, %v; want partial with b failed", got, err)
	}
	if kept, _ := fs.GetJobRun(ctx, "run_0"); kept.Status != models.JobRunOK {
		t.Errorf("a finished run was changed: %+v", kept)
	}
}
