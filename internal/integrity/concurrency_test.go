package integrity

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestConcurrentRestoreTestsOfOneDatabase(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	ctx := context.Background()
	job2 := &models.Job{ID: "job_2", Name: "second", Database: "shop", CronExpression: "@hourly", ConnectionID: "conn_src"}
	if err := f.st.CreateJob(ctx, job2); err != nil {
		t.Fatal(err)
	}
	rec2 := f.putBackup(t, "bkp_2", job2.ID, f.now.Add(-time.Minute), []byte("archive 2"), nil)

	// Both restores are in flight at the same time (same source, same second).
	var entered sync.WaitGroup
	entered.Add(2)
	f.restorer.execute = func(context.Context, models.RestoreRequest) error {
		entered.Done()
		entered.Wait()
		return nil
	}
	results := make([]*models.RestoreTestResult, 2)
	pairs := []struct {
		job *models.Job
		rec *models.BackupRecord
	}{{job, rec}, {job2, rec2}}
	var wg sync.WaitGroup
	for i, pair := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.svc.runRestoreTest(ctx, pair.job, pair.rec, TriggerManual)
		}()
	}
	wg.Wait()

	a, b := results[0].TempDatabase, results[1].TempDatabase
	if a == "" || a == b || !models.IsRescueVerifyDatabaseName(a) || !models.IsRescueVerifyDatabaseName(b) {
		t.Fatalf("temporary databases %q and %q must be distinct restore test names", a, b)
	}
	for _, r := range results {
		if r.Status == models.RestoreTestError || !r.Dropped {
			t.Fatalf("result = %+v", r)
		}
	}
	// Each test restored into, and dropped, exactly its own database.
	for _, req := range f.restorer.requests {
		want := a
		if req.BackupID == rec2.ID {
			want = b
		}
		if req.CloneDatabase != want {
			t.Fatalf("backup %s restored into %s, its test created %s", req.BackupID, req.CloneDatabase, want)
		}
	}
	dropped := slices.Clone(f.admin.dropped)
	slices.Sort(dropped)
	want := []string{a, b}
	slices.Sort(want)
	if !slices.Equal(dropped, want) {
		t.Fatalf("dropped %v, want exactly %v", dropped, want)
	}
	neverTouchedSource(t, f, "shop")
}

func TestParallelImportsOfOneKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "shop/2026/09/bkp_shop_20260901_030000_cafe.archive.gz"
	f.putObject(t, key, "orphan")

	const n = 8
	errs := make([]error, n)
	var start, wg sync.WaitGroup
	start.Add(1)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			_, errs[i] = f.svc.StartImport(ctx, "tgt_local", key)
		}()
	}
	start.Done()
	wg.Wait()
	f.waitIdle(t)

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrBusy), errors.Is(err, ErrNotOrphan):
		default:
			t.Fatalf("unexpected import error: %v", err)
		}
	}
	records, _ := f.st.ListBackupRecords(ctx, "")
	owners := 0
	for _, r := range records {
		if r.StorageKey == key {
			owners++
		}
	}
	if ok != 1 || owners != 1 {
		t.Fatalf("%d imports succeeded and %d records own the key; want 1 and 1", ok, owners)
	}
}
