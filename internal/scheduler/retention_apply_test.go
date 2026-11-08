package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestApplyRetentionRetainsCopiesAndAppliesBuckets proves the scheduler wiring: after
// a successful scheduled backup retention deletes by the job's rules (here the
// count, with keep_daily naming the newest) and marks the copies its override keeps
// as retained.
func TestApplyRetentionRetainsCopiesAndAppliesBuckets(t *testing.T) {
	ctx := context.Background()
	metaStore := storetest.New(t)
	s := NewScheduler(metaStore, nil, storage.NewMockStorage(), nil)
	job := &models.Job{ID: "job_w", Database: "shop", CronExpression: "@daily", Enabled: true, RetentionCount: 1, KeepDaily: 1,
		CopyTargets: []string{"tgt_lt"}, CopyRetention: map[string]models.RetentionPolicy{"tgt_lt": {RetentionDays: 365}}}
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	add := func(id string, age time.Duration) *models.BackupRecord {
		r := &models.BackupRecord{ID: id, JobID: job.ID, Database: "shop", Status: models.StatusCompleted,
			StorageKey: "shop/" + id + ".archive", StartedAt: now.Add(-age), SHA256: "aa"}
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_lt"}}, "")
		r.Copies[0].Status, r.Copies[0].SHA256 = models.CopyDone, "aa"
		if err := metaStore.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	cur := add("bkp_cur", 2*time.Hour)
	add("bkp_old1", 70*24*time.Hour)
	add("bkp_old2", 40*24*time.Hour)
	s.applyRetention(ctx, job, cur)
	for _, id := range []string{"bkp_old1", "bkp_old2"} {
		r, err := metaStore.GetBackupRecord(ctx, id)
		if err != nil || r.Status != models.StatusDeleted || !r.Copies[0].Retained {
			t.Fatalf("%s = %+v, %v; want deleted with a retained copy", id, r, err)
		}
	}
	if r, _ := metaStore.GetBackupRecord(ctx, "bkp_cur"); r.Status != models.StatusCompleted || r.Copies[0].Retained {
		t.Fatalf("the newest backup = %+v", r)
	}
}
