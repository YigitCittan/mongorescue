package operations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestPinAndUnpin(t *testing.T) {
	svc, st := newService(t)
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: &auth.User{Username: "alice"}, Method: auth.MethodSession, Scope: auth.ScopeAdmin})
	if err := st.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.PinBackup(ctx, "bkp_1", "  audit 2026  ")
	if err != nil || !rec.Pinned || rec.PinNote != "audit 2026" || rec.PinnedBy != "alice" || rec.PinnedAt == nil {
		t.Fatalf("pin = %+v, %v", rec, err)
	}
	if err = operations.CheckDeletable(rec); !errors.Is(err, operations.ErrPinned) {
		t.Fatalf("deleting a pinned backup = %v", err)
	}
	keyCtx := auth.WithPrincipal(context.Background(), &auth.Principal{APIKeyName: "ci", Method: auth.MethodAPIKey, Scope: auth.ScopeOperator})
	if rec, _ = svc.PinBackup(keyCtx, "bkp_1", ""); rec.PinnedBy != "API key ci" || rec.PinNote != "" {
		t.Fatalf("re-pin = %+v", rec)
	}
	rec, err = svc.UnpinBackup(ctx, "bkp_1")
	if err != nil || rec.Pinned || rec.PinnedAt != nil || operations.CheckDeletable(rec) != nil {
		t.Fatalf("unpin = %+v, %v", rec, err)
	}
	if _, err := svc.PinBackup(ctx, "nope", ""); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("unknown backup = %v", err)
	}
	if _, err := svc.PinBackup(ctx, "bkp_1", strings.Repeat("x", operations.MaxPinNoteLength+1)); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("long note = %v", err)
	}
	if _, err := svc.PinBackup(ctx, "bkp_1", "bad\x00note"); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("control characters = %v", err)
	}
}

func TestRetentionPreviewAndLog(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_r", Name: "r", Database: "shop", CronExpression: "@daily", RetentionCount: 2, StorageTargetID: "tgt"}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 5 {
		rec := &models.BackupRecord{ID: fmt.Sprintf("bkp_%d", i), JobID: job.ID, Trigger: models.TriggerScheduled, Database: "shop",
			Status: models.StatusCompleted, StorageTargetID: "tgt", StartedAt: now.Add(-time.Duration(i+1) * 48 * time.Hour), Pinned: i == 4}
		if err := st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	p, err := svc.RetentionPreview(ctx, job.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.RetentionCount != 2 || len(p.Delete) != 2 || p.Delete[0].Backup.ID != "bkp_3" || len(p.Protected) != 1 || p.Protected[0].BackupID != "bkp_4" {
		t.Fatalf("preview = %+v", p)
	}
	// Previewing unsaved values (the job form).
	days, count := 5, 0
	if p, err = svc.RetentionPreview(ctx, job.ID, &days, &count); err != nil || len(p.Delete) != 2 || p.RetentionDays != 5 {
		t.Fatalf("override preview = %+v, %v", p, err)
	}
	neg := -1
	if _, err = svc.RetentionPreview(ctx, job.ID, &neg, nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("negative = %v", err)
	}
	if _, err = svc.RetentionPreview(ctx, "nope", nil, nil); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}

	if err = st.AppendRetentionLog(ctx, &models.RetentionLogEntry{Time: now, JobID: job.ID, BackupID: "bkp_9", Reason: models.RetentionMaxCount}); err != nil {
		t.Fatal(err)
	}
	log, err := svc.RetentionLog(ctx, job.ID, 0)
	if err != nil || len(log) != 1 || log[0].BackupID != "bkp_9" {
		t.Fatalf("log = %+v, %v", log, err)
	}
}

func TestVerifyBackupUnavailableAndJobTrustFields(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	if _, err := svc.VerifyBackup(ctx, "bkp"); !errors.Is(err, operations.ErrUnavailable) {
		t.Fatalf("verify without integrity = %v", err)
	}
	job := &models.Job{ID: "job_v", Name: "v", Database: "shop", CronExpression: "@daily", ConnectionID: "c",
		LastRestoreTest: &models.RestoreTestSummary{ID: "rt_1", Status: models.RestoreTestOK}}
	if err := st.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	bad := models.VerifyOverride("sometimes")
	if _, err := svc.UpdateJob(ctx, job.ID, operations.JobUpdate{Database: "shop", ConnectionID: "c", VerifyAfterBackup: &bad}); err == nil {
		t.Fatal("an invalid override must be refused")
	}
	if _, err := svc.UpdateJob(ctx, job.ID, operations.JobUpdate{Database: "shop", ConnectionID: "c",
		RestoreTest: &models.RestoreTestPolicy{Enabled: true, Frequency: "hourly"}}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid restore test = %v", err)
	}
}
