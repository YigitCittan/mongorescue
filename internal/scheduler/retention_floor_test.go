package scheduler

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// retentionFixture is a scheduler with a job keeping 3 backups and 5 older completed
// backups of the job's database, each a few days old.
func retentionFixture(t *testing.T) (*Scheduler, store.Store, *models.Job) {
	t.Helper()
	metaStore := storetest.New(t)
	mock := storage.NewMockStorage()
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(runner))
	s := NewScheduler(metaStore, engine, mock, nil)

	ctx := context.Background()
	job := &models.Job{ID: "job_keep3", Database: "shop", CronExpression: "@daily", RetentionCount: 3, Enabled: true}
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 1; i <= 5; i++ {
		rec := &models.BackupRecord{
			ID: fmt.Sprintf("bkp_old_%d", i), JobID: job.ID, Database: job.Database, Status: models.StatusCompleted,
			StorageKey: fmt.Sprintf("shop/old_%d.gz", i), StartedAt: now.Add(-time.Duration(i) * 48 * time.Hour),
		}
		if err := metaStore.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if _, err := mock.Save(ctx, rec.StorageKey, strings.NewReader("archive")); err != nil {
			t.Fatal(err)
		}
	}
	return s, metaStore, job
}

// countStatus counts the backups of database in state status.
func countStatus(t *testing.T, st store.Store, database string, status models.BackupStatus) int {
	t.Helper()
	list, err := st.ListBackupRecords(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range list {
		if r.Status == status {
			n++
		}
	}
	return n
}

// TestOnDemandRunsNeverPrune proves that running a job repeatedly (REST POST
// /jobs/{id}/run, MCP run_job) cannot delete good backups through retention.
func TestOnDemandRunsNeverPrune(t *testing.T) {
	s, st, job := retentionFixture(t)
	ctx := context.Background()
	for i := range 7 {
		if i%2 == 0 {
			if _, err := s.TriggerJob(ctx, job.ID); err != nil {
				t.Fatalf("TriggerJob: %v", err)
			}
			continue
		}
		j, rec, err := s.PrepareJobRun(ctx, job.ID)
		if err != nil {
			t.Fatalf("PrepareJobRun: %v", err)
		}
		if _, err := s.ExecuteJobRun(ctx, j, rec); err != nil {
			t.Fatalf("ExecuteJobRun: %v", err)
		}
	}
	if n := countStatus(t, st, job.Database, models.StatusPruned); n != 0 {
		t.Fatalf("on-demand runs pruned %d backup(s); want none", n)
	}
	if n := countStatus(t, st, job.Database, models.StatusCompleted); n != 12 {
		t.Fatalf("completed backups = %d; want 12 (5 seeded + 7 runs)", n)
	}
}

// TestScheduledRunPrunes proves cron-triggered runs still apply retention.
func TestScheduledRunPrunes(t *testing.T) {
	s, st, job := retentionFixture(t)
	s.executeJob(context.Background(), job.ID)
	// 6 completed: the new one and the two newest seeded ones are kept.
	if n := countStatus(t, st, job.Database, models.StatusPruned); n != 3 {
		t.Fatalf("scheduled run pruned %d backup(s); want 3", n)
	}
	for _, id := range []string{"bkp_old_1", "bkp_old_2"} {
		rec, err := st.GetBackupRecord(context.Background(), id)
		if err != nil || rec.Status != models.StatusCompleted {
			t.Fatalf("%s = %+v, %v; want kept", id, rec, err)
		}
	}
}

// TestCountRetentionKeepsRecentBackups proves count-based retention never deletes a
// backup younger than MinCountPruneAge.
func TestCountRetentionKeepsRecentBackups(t *testing.T) {
	st := storetest.New(t)
	mock := storage.NewMockStorage()
	now := time.Now().UTC()
	var records []*models.BackupRecord
	for i := range 7 {
		r := &models.BackupRecord{
			ID: fmt.Sprintf("bkp_%d", i), Database: "shop", Status: models.StatusCompleted,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
		}
		records = append(records, r)
		_ = st.SaveBackupRecord(context.Background(), r)
	}
	pruned, err := PruneBackups(context.Background(), 0, 3, records, st, mock, nil)
	if err != nil || len(pruned) != 0 {
		t.Fatalf("pruned %v, %v; want nothing under a day old", pruned, err)
	}
}

// TestRetentionFloor proves the retention_count newest backups (at least one) are
// kept even when the time-based rule would expire them.
func TestRetentionFloor(t *testing.T) {
	cases := []struct {
		name        string
		days, count int
		wantPruned  int
	}{
		{"days with count floor", 1, 2, 1},
		{"days only keeps the newest", 1, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.New(t)
			now := time.Now().UTC()
			var records []*models.BackupRecord
			for i := range 3 {
				r := &models.BackupRecord{
					ID: fmt.Sprintf("bkp_%d", i), Database: "shop", Status: models.StatusCompleted,
					StartedAt: now.AddDate(0, 0, -5-i),
				}
				records = append(records, r)
				_ = st.SaveBackupRecord(context.Background(), r)
			}
			pruned, err := PruneBackups(context.Background(), tc.days, tc.count, records, st, storage.NewMockStorage(), nil)
			if err != nil || len(pruned) != tc.wantPruned {
				t.Fatalf("pruned %v, %v; want %d", pruned, err, tc.wantPruned)
			}
			for _, id := range pruned {
				if id == "bkp_0" {
					t.Fatal("the newest completed backup must never be pruned")
				}
			}
		})
	}
}
