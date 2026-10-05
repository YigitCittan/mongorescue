package operations_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// retryKeepsFilter retries failed backup id and checks that b is dumped again with
// --excludeCollection=logs.
func (e *multiEnv) retryKeepsFilter(t *testing.T, failed *models.BackupRecord) {
	t.Helper()
	if failed.Status != models.StatusFailed || !failed.Filtered || !slices.Equal(failed.ExcludedCollections, []string{"logs"}) {
		t.Fatalf("failed backup = %+v; want failed, filtered, excluding logs", failed)
	}
	e.mu.Lock()
	e.failing["b"] = false
	e.args["b"] = nil
	e.mu.Unlock()
	retried, err := e.svc.RetryBackup(context.Background(), failed.ID, models.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	done := e.awaitBackup(t, retried.ID)
	if done.Status != models.StatusCompleted || !slices.Equal(done.ExcludedCollections, []string{"logs"}) || !done.Filtered {
		t.Errorf("retried backup = %+v", done)
	}
	if got := e.dumpArgs("b", "--excludeCollection"); !slices.Equal(got, []string{"--excludeCollection=logs"}) {
		t.Errorf("retry of b dumped with %q; want --excludeCollection=logs", got)
	}
}

func TestRetryOfAnAdHocRunKeepsTheDatabasesFilter(t *testing.T) {
	e := newMultiEnv(t)
	e.failing["b"] = true
	run, err := e.svc.StartBackups(context.Background(), operations.BackupRequest{
		BackupOptions: models.BackupOptions{ConnectionID: "conn_a"},
		Databases:     []models.DatabaseFilter{{Name: "a"}, {Name: "b", ExcludeCollections: []string{"logs"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.awaitBackup(t, run.Backups[0].ID)
	e.retryKeepsFilter(t, e.awaitBackup(t, run.Backups[1].ID))
}

func TestRetryOfAJobRunKeepsTheDatabasesFilter(t *testing.T) {
	e := newMultiEnv(t, "a", "b")
	e.failing["b"] = true
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_f", Name: "filtered", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"},
			CollectionFilters: []models.DatabaseFilter{{Name: "b", ExcludeCollections: []string{"logs"}}}}})
	started, err := e.svc.RunJob(ctx, "job_f", models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	var failed *models.BackupRecord
	for deadline := time.Now().Add(5 * time.Second); failed == nil; time.Sleep(10 * time.Millisecond) {
		page, qErr := e.svc.QueryBackups(ctx, operations.BackupFilter{RunID: started.Run.ID})
		if qErr == nil {
			for _, b := range page.Items {
				if b.Database == "b" && b.Status == models.StatusFailed {
					failed = b.BackupRecord
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed backup of b: %+v, %v", page, qErr)
		}
	}
	// Wait for the run to end so b's lock is free again.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		list, _ := e.svc.ListJobRuns(ctx, "job_f", 1)
		if len(list) == 1 && list[0].Status != models.JobRunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish")
		}
	}
	e.retryKeepsFilter(t, failed)
}
