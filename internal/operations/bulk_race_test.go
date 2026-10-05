package operations_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// waitUsers waits until n callers hold or wait for the deletion lock key.
func waitUsers(t *testing.T, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runs.DeletionLockUsers(key) < n {
		if time.Now().After(deadline) {
			t.Fatalf("lock %s: %d users; want %d", key, runs.DeletionLockUsers(key), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// saveJobBackups stores the job and its backups (newest last in the arguments).
func saveJobBackups(t *testing.T, env *bulkEnv, jobID string, recs ...*models.BackupRecord) {
	t.Helper()
	ctx := context.Background()
	if err := env.st.SaveJob(ctx, &models.Job{ID: jobID, Name: jobID, Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		r.JobID, r.Database, r.Status, r.Trigger = jobID, "shop", models.StatusCompleted, models.TriggerScheduled
		if err := env.st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// markVerified records a successful verification of backup id.
func markVerified(t *testing.T, env *bulkEnv, id string) {
	t.Helper()
	if _, err := env.st.UpdateBackupRecord(context.Background(), id, func(r *models.BackupRecord) error {
		r.Verification = models.VerificationOK
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// verifiedLeft lists the job's completed, verified backups.
func verifiedLeft(t *testing.T, env *bulkEnv, jobID string) []string {
	t.Helper()
	list, err := env.st.ListBackupRecords(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range list {
		if r.JobID == jobID && r.Status == models.StatusCompleted && r.Verification == models.VerificationOK {
			out = append(out, r.ID)
		}
	}
	return out
}

// TestConcurrentBulkDeletesKeepAVerifiedBackup: bulk A plans while V1 is the job's
// only verified backup (so V2 looks deletable); V2 is then verified and bulk B plans
// (so V1 looks deletable). Both run concurrently: re-deciding the protections under
// the job's deletion lock must leave a verified backup.
func TestConcurrentBulkDeletesKeepAVerifiedBackup(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	saveJobBackups(t, env, "job_race",
		&models.BackupRecord{ID: "v1", StartedAt: now.Add(-72 * time.Hour), Verification: models.VerificationOK},
		&models.BackupRecord{ID: "v2", StartedAt: now.Add(-48 * time.Hour)},
		&models.BackupRecord{ID: "v3", StartedAt: now.Add(-24 * time.Hour)}, // the last good backup
	)
	key := runs.DeletionKey("job_race", "", "")
	unlock, err := runs.LockDeletion(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]*operations.BulkResult, 2)
	errs := make([]error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"v2"}})
	}()
	waitUsers(t, key, 2) // A planned v2 as deletable and waits for the lock
	markVerified(t, env, "v2")
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1], errs[1] = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"v1"}})
	}()
	waitUsers(t, key, 3) // B planned v1 as deletable too
	unlock()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("bulk %d: %v", i, err)
		}
	}
	if left := verifiedLeft(t, env, "job_race"); len(left) == 0 {
		t.Fatalf("no verified backup left: %+v / %+v", results[0], results[1])
	}
	if r := results[0]; r.Succeeded != 0 || len(r.Skipped) != 1 || r.Skipped[0].Reason != operations.SkipLastVerified {
		t.Fatalf("bulk A = %+v; want v2 skipped as the last verified backup", r)
	}
}

// TestBulkDeleteRacingRetention: a bulk delete planned while V0 was the newest verified
// backup races the retention that runs once V1 is verified (and prunes V0). V1 must
// survive whichever goes first.
func TestBulkDeleteRacingRetention(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	saveJobBackups(t, env, "job_ret",
		&models.BackupRecord{ID: "r0", StartedAt: now.Add(-96 * time.Hour), Verification: models.VerificationOK, StorageKey: "shop/r0"},
		&models.BackupRecord{ID: "r1", StartedAt: now.Add(-72 * time.Hour), StorageKey: "shop/r1"},
		&models.BackupRecord{ID: "r2", StartedAt: now.Add(-48 * time.Hour), StorageKey: "shop/r2"}, // the last good backup
	)
	key := runs.DeletionKey("job_ret", "", "")
	unlock, err := runs.LockDeletion(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var bulkRes *operations.BulkResult
	var bulkErr, pruneErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		bulkRes, bulkErr = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{"r1"}})
	}()
	waitUsers(t, key, 2)
	markVerified(t, env, "r1")
	records, err := env.st.ListBackupRecords(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, pruneErr = scheduler.PruneBackups(context.Background(), 0, 1, records, env.st, 7*24*time.Hour, nil)
	}()
	waitUsers(t, key, 3)
	unlock()
	wg.Wait()
	if bulkErr != nil || pruneErr != nil {
		t.Fatalf("bulk: %v, prune: %v", bulkErr, pruneErr)
	}
	left := verifiedLeft(t, env, "job_ret")
	if len(left) != 1 || left[0] != "r1" {
		t.Fatalf("verified backups left = %v; want r1 (bulk %+v)", left, bulkRes)
	}
}

// TestConcurrentDeletesOfSharedArchive: two records of different jobs share one
// archive and are deleted at the same time. Both deletions are soft (the archive
// stays for the grace period); the purge afterwards sees that no live record names
// the archive any more, so it is removed.
func TestConcurrentDeletesOfSharedArchive(t *testing.T) {
	env := newBulkEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	for _, job := range []string{"job_x", "job_y"} {
		saveJobBackups(t, env, job,
			&models.BackupRecord{ID: job + "_old", StartedAt: now.Add(-48 * time.Hour), StorageKey: "shop/shared"},
			&models.BackupRecord{ID: job + "_new", StartedAt: now.Add(-time.Hour)},
		)
	}
	if _, err := env.mock.Save(context.Background(), "shop/shared", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"job_x_old", "job_y_old"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res *operations.BulkResult
			res, errs[i] = env.svc.Bulk(ctx, operations.BulkBackups, operations.BulkRequest{Action: "delete", IDs: []string{id}})
			if errs[i] == nil && res.Succeeded != 1 {
				errs[i] = errFromResult(res)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !env.isDeleted("job_x_old") || !env.isDeleted("job_y_old") {
		t.Fatal("records not deleted")
	}
	if !env.hasObject("shop/shared") {
		t.Fatal("the shared archive was removed before the grace period")
	}
	storages := func(context.Context, string) (storage.Storage, error) { return env.mock, nil }
	grace := models.GraceDuration(models.DefaultDeleteGraceDays)
	if _, err := scheduler.PurgeDeleted(context.Background(), now.Add(grace+time.Hour), grace, env.st, storages, nil, nil); err != nil {
		t.Fatal(err)
	}
	if env.hasObject("shop/shared") {
		t.Fatal("the shared archive outlived both of its records' purge")
	}
}

// errFromResult describes an unexpected bulk result.
func errFromResult(res *operations.BulkResult) error {
	return &resultError{res}
}

type resultError struct{ res *operations.BulkResult }

func (e *resultError) Error() string {
	return "unexpected bulk result: " + strings.TrimSpace(strings.ReplaceAll(fmtResult(e.res), "\n", " "))
}

func fmtResult(res *operations.BulkResult) string {
	var b strings.Builder
	for _, r := range res.Results {
		b.WriteString(r.ID + ":" + r.Error + " ")
	}
	for _, s := range res.Skipped {
		b.WriteString(s.ID + " skipped " + s.Reason + " ")
	}
	return b.String()
}
