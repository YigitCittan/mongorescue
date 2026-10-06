package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

func TestCopiesKeepTheirTargetInUseAndQueue(t *testing.T) {
	s := storetest.OpenWithBox(t, filepath.Join(t.TempDir(), dbFile), testBox)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, id := range []string{"stg_p", "stg_c", "stg_j"} {
		tg := &models.StorageTarget{ID: id, Name: id, Type: models.StorageLocal, Local: &models.LocalTarget{Path: "/" + id}, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateStorageTarget(ctx, tg); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.SetDefaultStorageTarget(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec := &models.BackupRecord{ID: "bkp_1", Database: "db", Status: models.StatusCompleted, StorageTargetID: "stg_p",
		StorageKey: "db/k.archive.gz", StartedAt: now}
	rec.PlanCopies([]models.CopyTarget{{ID: "stg_c"}}, "")
	if err := s.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	job := &models.Job{ID: "job_1", Name: "j", Database: "db", StorageTargetID: "stg_p", CopyTargets: []string{"stg_j"}, CreatedAt: now}
	if err := s.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stg_c", "stg_j"} {
		if err := s.DeleteStorageTarget(ctx, id); !errors.Is(err, targets.ErrInUse) {
			t.Fatalf("delete %s = %v, want ErrInUse", id, err)
		}
	}
	pending, err := s.PendingCopyRecords(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "bkp_1" {
		t.Fatalf("PendingCopyRecords = %v, %v", pending, err)
	}
	keys, err := s.CopyKeys(ctx, "stg_c")
	if err != nil || !keys["db/k.archive.gz"] {
		t.Fatalf("CopyKeys = %v, %v", keys, err)
	}
	if _, err := s.UpdateBackupRecord(ctx, "bkp_1", func(r *models.BackupRecord) error {
		r.Copies[0].Status = models.CopyPurged
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageTarget(ctx, "stg_c"); err != nil {
		t.Fatalf("a target holding only purged copies must be deletable: %v", err)
	}
	if pending, _ = s.PendingCopyRecords(ctx); len(pending) != 0 {
		t.Fatalf("purged copies must leave the queue: %v", pending)
	}
}
