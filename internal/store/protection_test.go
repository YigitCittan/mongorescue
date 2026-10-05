package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

func TestPendingChangesReplaceAndTake(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	two, five := 2, 5
	first := &models.PendingChange{ID: "chg_1", Kind: models.PendingRetention, JobID: "job_a", RetentionCount: &five, CreatedAt: now, EffectiveAt: now.Add(48 * time.Hour)}
	if prev, err := s.ReplacePendingChange(ctx, first); err != nil || prev != nil {
		t.Fatalf("first change = %v, %v", prev, err)
	}
	grace := &models.PendingChange{ID: "chg_g", Kind: models.PendingDeleteGrace, DeleteGraceDays: &two, CreatedAt: now, EffectiveAt: now.Add(time.Hour)}
	if _, err := s.ReplacePendingChange(ctx, grace); err != nil {
		t.Fatal(err)
	}
	second := &models.PendingChange{ID: "chg_2", Kind: models.PendingRetention, JobID: "job_a", RetentionCount: &two, CreatedAt: now, EffectiveAt: now.Add(72 * time.Hour)}
	prev, err := s.ReplacePendingChange(ctx, second)
	if err != nil || prev == nil || prev.ID != "chg_1" {
		t.Fatalf("replace = %+v, %v; want chg_1 replaced", prev, err)
	}
	list, err := s.ListPendingChanges(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "chg_g" || list[1].ID != "chg_2" {
		t.Fatalf("list = %+v, %v; want chg_g then chg_2", list, err)
	}
	if err := s.DeletePendingChange(ctx, "chg_2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePendingChange(ctx, "chg_2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second take = %v; want ErrNotFound (a change is applied once)", err)
	}
	if ok, err := s.DeletePendingChangesOf(ctx, models.PendingDeleteGrace, ""); err != nil || !ok {
		t.Errorf("delete grace change = %v, %v", ok, err)
	}
}

func TestApprovalsUpdateAtomically(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	a := &models.Approval{ID: "apr_1", Action: models.ApprovalDeleteBackup, Status: models.ApprovalPending, Subject: "bkp_1",
		CreatedAt: now, ExpiresAt: now.Add(models.ApprovalTTL)}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApproval(ctx, a); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("duplicate = %v; want ErrAlreadyExists", err)
	}
	stop := errors.New("stop")
	if _, err := s.UpdateApproval(ctx, "apr_1", func(*models.Approval) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("aborted update = %v", err)
	}
	got, err := s.UpdateApproval(ctx, "apr_1", func(r *models.Approval) error { r.Status = models.ApprovalRejected; return nil })
	if err != nil || got.Status != models.ApprovalRejected {
		t.Fatalf("update = %+v, %v", got, err)
	}
	pending, err := s.ListApprovals(ctx, models.ApprovalPending, 10)
	if err != nil || len(pending) != 0 {
		t.Errorf("pending = %+v, %v; want none", pending, err)
	}
	all, err := s.ListApprovals(ctx, "", 10)
	if err != nil || len(all) != 1 || all[0].Status != models.ApprovalRejected {
		t.Errorf("all = %+v, %v", all, err)
	}
	if _, err := s.GetApproval(ctx, "apr_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

// TestDeletedBackupsKeepTargetInUse proves that a storage target cannot be deleted or
// moved while a deleted backup waits for its purge: only purged (and pruned) records
// free it.
func TestDeletedBackupsKeepTargetInUse(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	tgt := &models.StorageTarget{ID: "tgt_x", Name: "x", Type: models.StorageLocal, Local: &models.LocalTarget{Path: t.TempDir()}, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateStorageTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	rec := &models.BackupRecord{ID: "bkp_1", Database: "shop", Status: models.StatusCompleted, StorageTargetID: "tgt_x", StorageKey: "shop/k", StartedAt: now}
	rec.MarkDeleted(models.SoftDelete{At: now, PurgeAfter: now.Add(time.Hour), By: "admin"})
	if err := s.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStorageTarget(ctx, "tgt_x"); !errors.Is(err, targets.ErrInUse) {
		t.Fatalf("delete with a deleted backup = %v; want ErrInUse", err)
	}
	moved := *tgt
	moved.Local = &models.LocalTarget{Path: t.TempDir()}
	moved.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateStorageTarget(ctx, &moved, tgt.UpdatedAt, true); !errors.Is(err, targets.ErrLocationInUse) {
		t.Fatalf("move with a deleted backup = %v; want ErrLocationInUse", err)
	}
	failed := &models.BackupRecord{ID: "bkp_2", Database: "shop", Status: models.StatusFailed, StorageTargetID: "tgt_x", StartedAt: now}
	if err := s.SaveBackupRecord(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountStorageTargetBackups(ctx, "tgt_x"); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2 (any status but purged)", n, err)
	}
	for _, r := range []*models.BackupRecord{rec, failed} {
		r.Status = models.StatusPurged
		if err := s.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteStorageTarget(ctx, "tgt_x"); err != nil {
		t.Fatalf("delete once every backup is purged = %v", err)
	}
}

func TestBackupFilterExcludesStatuses(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	now := time.Now().UTC()
	for id, st := range map[string]models.BackupStatus{"a": models.StatusCompleted, "b": models.StatusDeleted, "c": models.StatusPurged} {
		if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, Database: "shop", Status: st, StartedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.QueryBackupRecords(ctx, store.BackupFilter{ExcludeStatuses: []models.BackupStatus{models.StatusDeleted, models.StatusPurged}})
	if err != nil || page.Total != 1 || page.Rows[0].Record.ID != "a" {
		t.Fatalf("page = %+v, %v; want only a", page, err)
	}
}
