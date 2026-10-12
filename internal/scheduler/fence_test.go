package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// lostFence is a fence of an instance that lost the HA lease.
type lostFence struct{}

func (lostFence) Check() error  { return errors.New("this instance does not hold the HA lease") }
func (lostFence) Epoch() uint64 { return 0 }

// TestMaintenanceAndRetentionNeedTheFence checks that an instance that lost the HA
// lease neither purges nor applies retention.
func TestMaintenanceAndRetentionNeedTheFence(t *testing.T) {
	ctx := context.Background()
	metaStore := storetest.New(t)
	var maintained atomic.Int32
	s := NewScheduler(metaStore, backup.NewEngine(storage.NewMockStorage(), "mongodb://x"), storage.NewMockStorage(), nil,
		WithFence(lostFence{}), WithMaintenance(func(context.Context) { maintained.Add(1) }))
	s.Maintain(ctx)
	if maintained.Load() != 0 {
		t.Fatal("maintenance ran without the HA lease")
	}

	job := &models.Job{ID: "job_1", Database: "shop", CronExpression: "@daily", Enabled: true, RetentionCount: 1}
	now := time.Now().UTC()
	for i, id := range []string{"bkp_old", "bkp_new"} {
		rec := &models.BackupRecord{ID: id, JobID: job.ID, Database: "shop", Status: models.StatusCompleted, Trigger: models.TriggerScheduled,
			StartedAt: now.Add(time.Duration(i-1) * 48 * time.Hour), StorageKey: "shop/" + id + ".archive"}
		if err := metaStore.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	newest, err := metaStore.GetBackupRecord(ctx, "bkp_new")
	if err != nil {
		t.Fatal(err)
	}
	s.applyRetention(ctx, job, newest)
	if old, _ := metaStore.GetBackupRecord(ctx, "bkp_old"); old.Status != models.StatusCompleted {
		t.Fatalf("retention deleted a backup without the HA lease: %s", old.Status)
	}
	// With the lease the same retention deletes it, so the check above is not vacuous.
	holding := NewScheduler(metaStore, backup.NewEngine(storage.NewMockStorage(), "mongodb://x"), storage.NewMockStorage(), nil)
	holding.applyRetention(ctx, job, newest)
	if old, _ := metaStore.GetBackupRecord(ctx, "bkp_old"); old.Status == models.StatusCompleted {
		t.Fatal("retention kept the old backup with the lease too; the test does not exercise retention")
	}
}
