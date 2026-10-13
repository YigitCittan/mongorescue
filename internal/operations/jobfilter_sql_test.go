package operations_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// filterEnv stores jobs of every schedule, selection and last status on two
// connections; job_moved belongs to c2 but its newest backup was taken from c1.
func filterEnv(t *testing.T) *bulkEnv {
	t.Helper()
	env := newBulkEnv(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, j := range []*models.Job{
		{ID: "job_hourly", Name: "Orders hourly", Database: "orders", CronExpression: "0 * * * *", ConnectionID: "c1"},
		{ID: "job_daily", Name: "Shop nightly", Database: "shop", CronExpression: "0 3 * * *", ConnectionID: "c1"},
		{ID: "job_weekly", Name: "CRM weekly", Database: "crm", CronExpression: "0 3 * * 1", ConnectionID: "c2"},
		{ID: "job_monthly", Name: "Ledger", Database: "ledger", CronExpression: "0 3 1 * *", ConnectionID: "c2"},
		{ID: "job_other", Name: "Archive yearly", Database: "archive", CronExpression: "0 3 1 1 *", ConnectionID: "c1"},
		{ID: "job_list", Name: "Pair", CronExpression: "@daily", ConnectionID: "c1",
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"shop", "crm"}}},
		{ID: "job_pattern", Name: "Tenants", CronExpression: "@every 6h", ConnectionID: "c2", KnownDatabases: []string{"tenant_a"},
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"tenant_*"}, Exclude: []string{"tenant_x"}}},
		{ID: "job_all", Name: "Everything", CronExpression: "@weekly", ConnectionID: "c1",
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll, Exclude: []string{"scratch"}}},
		{ID: "job_moved", Name: "Moved shop", Database: "shop", CronExpression: "0 4 * * *", ConnectionID: "c2"},
	} {
		j.Enabled = j.ID != "job_monthly"
		if err := env.st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	backup := func(id, job, conn string, st models.BackupStatus, ago time.Duration) {
		t.Helper()
		if err := env.st.SaveBackupRecord(ctx, &models.BackupRecord{ID: id, JobID: job, Database: "shop", ConnectionID: conn, Status: st,
			StartedAt: now.Add(-ago)}); err != nil {
			t.Fatal(err)
		}
	}
	backup("b1", "job_hourly", "c1", models.StatusFailed, time.Hour)
	backup("b2", "job_daily", "c1", models.StatusCompleted, 2*time.Hour)
	backup("b3", "job_weekly", "c2", models.StatusCancelled, 3*time.Hour)
	backup("b4", "job_monthly", "c2", models.StatusInProgress, 4*time.Hour)
	backup("b5", "job_moved", "c2", models.StatusCompleted, 5*time.Hour)
	backup("b6", "job_moved", "c1", models.StatusFailed, time.Minute)
	if err := env.st.SaveJobRun(ctx, &models.JobRun{ID: "r1", JobID: "job_list", Status: models.JobRunPartial, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	return env
}

// listAll pages through ListJobsPage with filter f in order sortBy, returning
// the IDs and the total of the first page.
func listAll(ctx context.Context, t *testing.T, svc *operations.Service, f operations.BulkFilter, sortBy string, desc bool) ([]string, int) {
	t.Helper()
	var ids []string
	total := -1
	req := operations.JobListRequest{Filter: f, Sort: sortBy, Desc: desc, Limit: 2}
	for range 100 {
		p, err := svc.ListJobsPage(ctx, req)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		if total < 0 {
			total = p.Total
		}
		for _, j := range p.Items {
			ids = append(ids, j.ID)
		}
		if p.Next == "" {
			return ids, total
		}
		req.After = p.Next
	}
	t.Fatal("paging did not end")
	return nil, 0
}

// TestJobListFiltersInSQLMatchFilterJobs checks that the jobs list answered from
// the jobs' search, schedule, database and last status columns returns exactly
// the jobs FilterJobs (the bulk actions' matcher) does, for every caller: a
// caller limited to c2, whose moved job's newest backup comes from c1, gets the
// same answer through the matcher.
func TestJobListFiltersInSQLMatchFilterJobs(t *testing.T) {
	env := filterEnv(t)
	on, off := true, false
	var filters []operations.BulkFilter
	for _, q := range []string{"", "shop", "SHOP", "job_", "tenant", "list shop", "pattern tenant_*", "nothing"} {
		filters = append(filters, operations.BulkFilter{Q: q})
	}
	for _, db := range []string{"shop", "crm", "tenant_a", "tenant_b", "tenant_x", "admin", "scratch", "anything"} {
		filters = append(filters, operations.BulkFilter{Database: db})
	}
	for _, s := range operations.JobFrequencies {
		filters = append(filters, operations.BulkFilter{Schedule: s})
	}
	for _, s := range operations.JobLastStatuses {
		filters = append(filters, operations.BulkFilter{LastStatus: s}, operations.BulkFilter{LastStatus: s, Enabled: &on})
	}
	filters = append(filters,
		operations.BulkFilter{Database: "shop", Schedule: operations.FrequencyDaily, LastStatus: operations.LastRunCompleted},
		operations.BulkFilter{Q: "o", ConnectionID: "c2", Enabled: &off},
		operations.BulkFilter{Database: "tenant_b", ConnectionID: "c2", Q: "ten"},
	)
	for name, ctx := range map[string]context.Context{
		"admin": admin(), "c1": limitedTo(auth.ScopeRead, "c1"), "c2": limitedTo(auth.ScopeRead, "c2"),
	} {
		for _, f := range filters {
			want, err := env.svc.FilterJobs(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			var wantIDs []string
			for _, j := range want {
				wantIDs = append(wantIDs, j.ID)
			}
			got, total := listAll(ctx, t, env.svc, f, operations.JobSortName, false)
			if !slices.Equal(got, wantIDs) || total != len(wantIDs) {
				t.Errorf("%s %+v: list %v (total %d); FilterJobs %v", name, f, got, total, wantIDs)
			}
		}
	}
}

// TestJobListLastStatusOrderForEveryCaller checks the last-status order (failures
// first, the newest first within an outcome, never ran last) in both directions,
// from the stored last status and, for a caller whose job's newest backup is on
// another connection, from the backups it may see.
func TestJobListLastStatusOrderForEveryCaller(t *testing.T) {
	env := filterEnv(t)
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		asc, desc []string
	}{
		{"admin", admin(),
			[]string{"job_moved", "job_hourly", "job_list", "job_weekly", "job_monthly", "job_daily", "job_all", "job_other", "job_pattern"},
			[]string{"job_daily", "job_monthly", "job_weekly", "job_list", "job_hourly", "job_moved", "job_pattern", "job_other", "job_all"}},
		// c2 sees job_moved's backup from c2 only: completed five hours ago.
		{"c2", limitedTo(auth.ScopeRead, "c2"),
			[]string{"job_weekly", "job_monthly", "job_moved", "job_pattern"},
			[]string{"job_moved", "job_monthly", "job_weekly", "job_pattern"}},
	} {
		for _, desc := range []bool{false, true} {
			want := tc.asc
			if desc {
				want = tc.desc
			}
			if got, total := listAll(tc.ctx, t, env.svc, operations.BulkFilter{}, operations.JobSortLastStatus, desc); !slices.Equal(got, want) || total != len(want) {
				t.Errorf("%s desc=%v: %v (total %d); want %v", tc.name, desc, got, total, want)
			}
		}
	}
}
