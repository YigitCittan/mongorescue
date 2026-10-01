package events

import (
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestVerificationEvent(t *testing.T) {
	if _, ok := VerificationEvent(nil, VerificationSweep); ok {
		t.Fatal("nil record")
	}
	if _, ok := VerificationEvent(&models.BackupRecord{ID: "b"}, VerificationSweep); ok {
		t.Fatal("unverified record")
	}
	at := time.Unix(1_790_000_000, 0).UTC()
	e, ok := VerificationEvent(&models.BackupRecord{ID: "b", JobID: "j", Database: "shop", Verification: models.VerificationOK, VerifiedAt: &at}, VerificationAfterUpload)
	if !ok || e.Type != VerificationSucceeded || !e.Time.Equal(at) || e.Source != VerificationAfterUpload || e.Error != "" {
		t.Fatalf("ok event = %+v", e)
	}
	e, _ = VerificationEvent(&models.BackupRecord{ID: "b", Verification: models.VerificationMismatch,
		VerificationError: "read mongodb://u:hunter2@h/x failed"}, VerificationSweep)
	if e.Type != VerificationFailed || strings.Contains(e.Error, "hunter2") || !e.Type.Failed() || !e.Type.Subscribable() {
		t.Fatalf("failed event = %+v", e)
	}
	if VerificationSucceeded.Subscribable() {
		t.Fatal("verification.succeeded feeds metrics only")
	}
}

func TestRestoreTestDriftAndRetentionEvents(t *testing.T) {
	done := time.Now().UTC()
	e := RestoreTestEvent(&models.RestoreTestResult{JobID: "j", BackupID: "b", Status: models.RestoreTestMismatch, Trigger: "scheduled",
		Mismatches: []string{"a", "b"}, CompletedAt: &done, DurationSeconds: 2, DropError: "boom", TempDatabase: "t"})
	if e.Type != RestoreTestFailed || e.Error != "a; b" || e.Duration != 2*time.Second || !strings.Contains(e.Detail, "boom") {
		t.Fatalf("restore test event = %+v", e)
	}
	if e = RestoreTestEvent(&models.RestoreTestResult{Status: models.RestoreTestOK}); e.Type != RestoreTestSucceeded || e.Error != "" {
		t.Fatalf("ok restore test = %+v", e)
	}
	d := DriftEvent("tgt", "Local", 2, 1, "scheduled", done)
	if d.Type != DriftDetected || d.Orphans != 2 || d.Missing != 1 || !strings.Contains(d.Detail, "2 orphan") {
		t.Fatalf("drift = %+v", d)
	}
	r := RetentionEvent(models.RetentionLogEntry{JobID: "j", BackupID: "b", Detail: "older than 3 days", Time: done})
	if r.Type != RetentionDeleted || r.Status != "pruned" || !r.Type.Subscribable() || r.Type.Failed() {
		t.Fatalf("retention = %+v", r)
	}
}
