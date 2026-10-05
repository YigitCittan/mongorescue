package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestPruneBackupRecordRechecksProtection(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	save := func(id string, age time.Duration, mutate func(*models.BackupRecord)) {
		r := &models.BackupRecord{ID: id, JobID: "job_1", Trigger: models.TriggerScheduled, Database: "shop",
			Status: models.StatusCompleted, StorageTargetID: "tgt", StartedAt: now.Add(-age)}
		if mutate != nil {
			mutate(r)
		}
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	verified := func(r *models.BackupRecord) { r.Verification = models.VerificationOK }
	save("old_verified", 72*time.Hour, verified)
	save("new_verified", 48*time.Hour, verified)
	save("pinned", 96*time.Hour, func(r *models.BackupRecord) { r.Pinned = true })
	save("failed", 96*time.Hour, func(r *models.BackupRecord) { r.Status = models.StatusFailed })
	save("other_target_verified", time.Hour, func(r *models.BackupRecord) { r.Verification, r.StorageTargetID = models.VerificationOK, "tgt2" })

	for _, id := range []string{"new_verified", "pinned", "failed"} {
		if _, err := s.PruneBackupRecord(ctx, id, nil); !errors.Is(err, store.ErrPruneRefused) {
			t.Errorf("prune %s = %v, want ErrPruneRefused", id, err)
		}
	}
	// A verified backup with a newer verified one (same target) may go.
	if rec, err := s.PruneBackupRecord(ctx, "old_verified", nil); err != nil || rec.Status != models.StatusPruned {
		t.Fatalf("prune old_verified = %+v, %v", rec, err)
	}
	if _, err := s.PruneBackupRecord(ctx, "nope", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if rec, _ := s.GetBackupRecord(ctx, "new_verified"); rec.Status != models.StatusCompleted {
		t.Fatalf("the newest verified backup must stay: %s", rec.Status)
	}
}
