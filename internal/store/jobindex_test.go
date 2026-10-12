package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// matching returns the IDs of the jobs of q in name order, after checking that
// CountJobs agrees.
func matching(t *testing.T, s *store.SQLiteStore, q store.JobQuery) []string {
	t.Helper()
	ids := pageAll(t, s, q, 2)
	n, err := s.CountJobs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(ids) {
		t.Fatalf("CountJobs(%+v) = %d; the pages have %d jobs", q, n, len(ids))
	}
	return ids
}

// TestJobSearchFollowsJobWrites checks that the search text and the databases
// column follow every job write: creation, update, the known databases a run
// records, the upsert of SaveJob, and deletion.
func TestJobSearchFollowsJobWrites(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_1", Name: "Nightly Shop", Database: "shop", CronExpression: "0 3 * * *", Enabled: true}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	search := func(q string) []string { return matching(t, s, store.JobQuery{Search: q}) }
	for _, q := range []string{"nightly", "SHOP", "job_1", "single", "ly sh"} {
		if got := search(q); !slices.Equal(got, []string{"job_1"}) {
			t.Errorf("search %q after create = %v", q, got)
		}
	}
	// A search never spans two fields.
	if got := search("shopjob"); len(got) != 0 {
		t.Errorf("search across fields = %v", got)
	}

	job.Name = "Hourly Orders"
	job.CronExpression = "0 * * * *"
	if err := s.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got := search("nightly"); len(got) != 0 {
		t.Errorf("the old name still matches: %v", got)
	}
	if got := search("orders"); !slices.Equal(got, []string{"job_1"}) {
		t.Errorf("the new name: %v", got)
	}
	if got := matching(t, s, store.JobQuery{Schedule: models.FrequencyHourly}); !slices.Equal(got, []string{"job_1"}) {
		t.Errorf("schedule after update: %v", got)
	}

	// A pattern job: its known databases (recorded by a run) are searched and
	// filtered on, and its patterns match databases it does not know yet.
	multi := &models.Job{ID: "job_2", Name: "tenants", CronExpression: "@daily", Enabled: true,
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"tenant_*"}}}
	if err := s.SaveJob(ctx, multi); err != nil {
		t.Fatal(err)
	}
	if got := search("acme"); len(got) != 0 {
		t.Errorf("an unknown database matched: %v", got)
	}
	if err := s.UpdateJobKnownDatabases(ctx, "job_2", func(*models.Job) ([]string, bool) { return []string{"tenant_acme"}, true }); err != nil {
		t.Fatal(err)
	}
	if got := search("acme"); !slices.Equal(got, []string{"job_2"}) {
		t.Errorf("search of a known database: %v", got)
	}
	for db, want := range map[string][]string{
		"shop": {"job_1"}, "tenant_acme": {"job_2"}, "tenant_new": {"job_2"}, "tenant": nil, "admin": nil,
	} {
		if got := matching(t, s, store.JobQuery{Database: db}); !slices.Equal(got, want) {
			t.Errorf("database %q = %v; want %v", db, got, want)
		}
	}

	// SaveJob's upsert of an existing job.
	multi.Name = "customers"
	if err := s.SaveJob(ctx, multi); err != nil {
		t.Fatal(err)
	}
	if got := search("tenants"); len(got) != 0 {
		t.Errorf("the upserted job's old name matches: %v", got)
	}

	if err := s.DeleteJob(ctx, "job_1"); err != nil {
		t.Fatal(err)
	}
	if got := search("orders"); len(got) != 0 {
		t.Errorf("a deleted job matches: %v", got)
	}
	if _, err := s.QueryJobs(ctx, store.JobQuery{Search: "a" + store.JobSearchSeparator}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Errorf("a search with the separator: %v", err)
	}
	if _, err := s.QueryJobs(ctx, store.JobQuery{LastStatus: "exploded"}); !errors.Is(err, store.ErrInvalidFilter) {
		t.Errorf("an unknown last status: %v", err)
	}
}

// lastStatusEnv stores jobs with every last status; times are minutes before now.
func lastStatusEnv(t *testing.T) (*store.SQLiteStore, time.Time) {
	t.Helper()
	s := storetest.New(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, j := range []*models.Job{
		{ID: "j_failed", Name: "a", Database: "shop", ConnectionID: "c1"},
		{ID: "j_ok", Name: "b", Database: "shop", ConnectionID: "c1"},
		{ID: "j_never", Name: "c", Database: "shop", ConnectionID: "c2"},
		{ID: "j_multi", Name: "d", ConnectionID: "c2", DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"x", "y"}}},
		{ID: "j_running", Name: "e", Database: "shop", ConnectionID: "c2"},
	} {
		j.CronExpression, j.Enabled = "@daily", true
		if err := s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	backup := func(id, job string, st models.BackupStatus, minutes int) {
		t.Helper()
		if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, JobID: job, Database: "shop", ConnectionID: "c1", Status: st,
			StartedAt: now.Add(-time.Duration(minutes) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	backup("b1", "j_failed", models.StatusCompleted, 60)
	backup("b2", "j_failed", models.StatusFailed, 30)
	backup("b3", "j_ok", models.StatusCompleted, 10)
	backup("b4", "j_running", models.StatusInProgress, 5)
	// A multi-database job's last status is its run's, never one database's backup.
	backup("b5", "j_multi", models.StatusFailed, 1)
	for _, r := range []*models.JobRun{
		{ID: "r1", JobID: "j_multi", Status: models.JobRunPartial, StartedAt: now.Add(-20 * time.Minute)},
		{ID: "r2", JobID: "j_multi", Status: models.JobRunSkipped, StartedAt: now.Add(-2 * time.Minute)},
	} {
		if err := s.SaveJobRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return s, now
}

// TestJobLastStatusFollowsBackupsAndRuns checks the last status the triggers keep
// on the jobs: filtered, ordered both ways and paged, and updated by backup and run
// inserts, status changes and deletions.
func TestJobLastStatusFollowsBackupsAndRuns(t *testing.T) {
	s, now := lastStatusEnv(t)
	ctx := context.Background()
	for status, want := range map[string][]string{
		store.JobLastFailed: {"j_failed"}, store.JobLastPartial: {"j_multi"}, store.JobLastCompleted: {"j_ok"},
		store.JobLastInProgress: {"j_running"}, store.JobLastNever: {"j_never"}, store.JobLastCancelled: nil,
	} {
		if got := matching(t, s, store.JobQuery{LastStatus: status}); !slices.Equal(got, want) {
			t.Errorf("last status %s = %v; want %v", status, got, want)
		}
	}
	order := func(desc bool) []string {
		return pageAll(t, s, store.JobQuery{Sort: store.JobSortLastStatus, Desc: desc}, 2)
	}
	if got, want := order(false), []string{"j_failed", "j_multi", "j_running", "j_ok", "j_never"}; !slices.Equal(got, want) {
		t.Errorf("ascending = %v; want %v", got, want)
	}
	if got, want := order(true), []string{"j_ok", "j_running", "j_multi", "j_failed", "j_never"}; !slices.Equal(got, want) {
		t.Errorf("descending = %v; want %v", got, want)
	}

	// The running backup completes, a newer one fails; deleting it brings the
	// completed one back.
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b4", JobID: "j_running", Database: "shop", Status: models.StatusCompleted,
		StartedAt: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastCompleted}); !slices.Equal(got, []string{"j_ok", "j_running"}) {
		t.Errorf("after completing b4: %v", got)
	}
	if err := s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b6", JobID: "j_running", Database: "shop", Status: models.StatusCancelled,
		StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastCancelled}); !slices.Equal(got, []string{"j_running"}) {
		t.Errorf("after a cancelled backup: %v", got)
	}
	if err := s.DeleteBackupRecord(ctx, "b6"); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastCancelled}); len(got) != 0 {
		t.Errorf("after deleting the cancelled backup: %v", got)
	}
	// A new run of the multi-database job.
	if err := s.SaveJobRun(ctx, &models.JobRun{ID: "r3", JobID: "j_multi", Status: models.JobRunOK, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastPartial}); len(got) != 0 {
		t.Errorf("after an ok run: %v", got)
	}
	// Becoming a single-database job, j_multi takes its newest backup's status.
	if err := s.SaveJob(ctx, &models.Job{ID: "j_multi", Name: "d", Database: "x", ConnectionID: "c2", CronExpression: "@daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastFailed}); !slices.Equal(got, []string{"j_failed", "j_multi"}) {
		t.Errorf("after j_multi became single-database: %v", got)
	}
	// A job created anew under the ID of a deleted job takes its backups' status.
	if err := s.DeleteJob(ctx, "j_ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJob(ctx, &models.Job{ID: "j_ok", Name: "b", Database: "shop", CronExpression: "@daily"}); err != nil {
		t.Fatal(err)
	}
	if got := matching(t, s, store.JobQuery{LastStatus: store.JobLastCompleted}); !slices.Contains(got, "j_ok") {
		t.Errorf("the recreated job: %v", got)
	}
}

// TestJobLastStatusPagesFromEveryCursor checks that every page of the last-status
// order starts right after the previous one, with the connection and state
// filters, against CompareJobKeys.
func TestJobLastStatusPagesFromEveryCursor(t *testing.T) {
	s, _ := lastStatusEnv(t)
	ctx := context.Background()
	on := true
	for _, base := range []store.JobQuery{{}, {ConnectionID: "c2"}, {Enabled: &on}, {Connections: auth.OnlyConnections("c1", "c2")}} {
		for _, desc := range []bool{false, true} {
			q := base
			q.Sort, q.Desc = store.JobSortLastStatus, desc
			all := pageAll(t, s, q, 100)
			for size := 1; size <= len(all); size++ {
				if got := pageAll(t, s, q, size); !slices.Equal(got, all) {
					t.Errorf("%+v desc=%v by %d = %v; want %v", base, desc, size, got, all)
				}
			}
			// The cursors of windows of one job are in CompareJobKeys order.
			var keys []store.JobKey
			one := q
			one.Limit = 1
			for {
				w, err := s.QueryJobs(ctx, one)
				if err != nil {
					t.Fatal(err)
				}
				if len(w.Jobs) == 0 {
					break
				}
				keys = append(keys, w.Last)
				last := w.Last
				one.After = &last
			}
			for i := 1; i < len(keys); i++ {
				if store.CompareJobKeys(keys[i-1], keys[i], desc) >= 0 {
					t.Errorf("%+v desc=%v: key %+v before %+v", base, desc, keys[i-1], keys[i])
				}
			}
		}
	}
}

// TestHasForeignLastBackups checks the detection of a job whose newest backup was
// taken from another connection than the caller's.
func TestHasForeignLastBackups(t *testing.T) {
	s, _ := lastStatusEnv(t)
	ctx := context.Background()
	// j_running (connection c2) has its newest backup from c1.
	for _, tc := range []struct {
		set  auth.ConnectionSet
		want bool
	}{
		{nil, false}, {auth.OnlyConnections("c1"), false}, {auth.OnlyConnections("c2"), true},
		{auth.OnlyConnections("c1", "c2"), false}, {auth.OnlyConnections(), false},
	} {
		if got, err := s.HasForeignLastBackups(ctx, tc.set); err != nil || got != tc.want {
			t.Errorf("HasForeignLastBackups(%v) = %v, %v; want %v", tc.set.IDs(), got, err, tc.want)
		}
	}
}

// TestChangeStampCountsEveryWrite checks that the change stamp moves on every
// write to the tables the overview and the readiness report read, deletions
// included, and not on reads.
func TestChangeStampCountsEveryWrite(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	last, err := s.ChangeStamp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	step := func(what string, fn func() error) {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		n, err := s.ChangeStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == last {
			t.Errorf("%s left the change stamp at %d", what, n)
		}
		last = n
	}
	now := time.Now().UTC()
	job := &models.Job{ID: "job_1", Name: "a", Database: "shop", CronExpression: "@daily", Enabled: true}
	step("create job", func() error { return s.CreateJob(ctx, job) })
	step("update job", func() error { return s.UpdateJob(ctx, job) })
	step("run times", func() error { return s.UpdateJobRunTimes(ctx, "job_1", &now, nil) })
	step("save backup", func() error {
		return s.SaveBackupRecord(ctx, &models.BackupRecord{ID: "b1", JobID: "job_1", Database: "shop", Status: models.StatusCompleted, StartedAt: now})
	})
	step("save run", func() error {
		return s.SaveJobRun(ctx, &models.JobRun{ID: "r1", JobID: "job_1", Status: models.JobRunOK, StartedAt: now})
	})
	step("save restore", func() error {
		return s.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "r1", BackupID: "b1", SourceDatabase: "shop", TargetDatabase: "shop_rescue",
			Status: models.RestoreStatusCompleted, StartedAt: now})
	})
	step("add rpo breach", func() error {
		_, err := s.AddRPOBreach(ctx, models.RPOBreach{JobID: "job_1", Database: "shop", Since: now})
		return err
	})
	step("delete rpo breach", func() error { _, err := s.DeleteRPOBreach(ctx, "job_1", "shop"); return err })
	step("delete restore", func() error { return s.DeleteRestoreRecord(ctx, "r1") })
	step("delete backup", func() error { return s.DeleteBackupRecord(ctx, "b1") })
	step("delete job", func() error { return s.DeleteJob(ctx, "job_1") })

	if _, err := s.ListJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackupStats(ctx, now); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ChangeStamp(ctx); n != last {
		t.Errorf("reads moved the change stamp from %d to %d", last, n)
	}
}
