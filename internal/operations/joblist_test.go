package operations_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestScheduleFrequency(t *testing.T) {
	for expr, want := range map[string]string{
		"@hourly":               operations.FrequencyHourly,
		"@every 30m":            operations.FrequencyHourly,
		"@every 6h":             operations.FrequencyHourly,
		"*/15 * * * *":          operations.FrequencyHourly,
		"0 * * * *":             operations.FrequencyHourly,
		"0 */6 * * *":           operations.FrequencyHourly,
		"0 2,14 * * *":          operations.FrequencyHourly,
		"@daily":                operations.FrequencyDaily,
		"@midnight":             operations.FrequencyDaily,
		"@every 24h":            operations.FrequencyDaily,
		"30 2 * * *":            operations.FrequencyDaily,
		"CRON_TZ=UTC 0 3 * * *": operations.FrequencyDaily,
		"0 3 ? * ?":             operations.FrequencyDaily,
		"@weekly":               operations.FrequencyWeekly,
		"@every 168h":           operations.FrequencyWeekly,
		"0 3 * * 0":             operations.FrequencyWeekly,
		"0 3 * * 1-5":           operations.FrequencyWeekly,
		"@monthly":              operations.FrequencyMonthly,
		"0 4 1 * *":             operations.FrequencyMonthly,
		"0 4 1,15 * *":          operations.FrequencyMonthly,
		"@yearly":               operations.FrequencyOther,
		"0 0 1 1 *":             operations.FrequencyOther,
		"0 0 1 * 1":             operations.FrequencyOther,
		"@every 36h":            operations.FrequencyOther,
		"@every nonsense":       operations.FrequencyOther,
		"TZ=UTC":                operations.FrequencyOther,
		"":                      operations.FrequencyOther,
		"* * *":                 operations.FrequencyOther,
	} {
		if got := operations.ScheduleFrequency(expr); got != want {
			t.Errorf("ScheduleFrequency(%q) = %q; want %q", expr, got, want)
		}
	}
}

// jobListEnv stores five jobs with different schedules, connections, states and
// last backups.
func jobListEnv(t *testing.T) *bulkEnv {
	t.Helper()
	env := newBulkEnv(t)
	ctx := admin()
	for _, j := range []*models.Job{
		{ID: "j_hourly", Name: "Orders hourly", Database: "shop", CronExpression: "0 * * * *", ConnectionID: "c1", Enabled: true},
		{ID: "j_daily", Name: "Nightly CRM", Database: "crm", CronExpression: "30 2 * * *", ConnectionID: "c2", Enabled: true},
		{ID: "j_weekly", Name: "Weekly shop", Database: "shop", CronExpression: "@weekly", ConnectionID: "c1"},
		{ID: "j_monthly", Name: "Archive", Database: "logs", CronExpression: "0 4 1 * *", ConnectionID: "c2", Enabled: true},
		{ID: "j_never", Name: "New", Database: "crm", CronExpression: "@daily", ConnectionID: "c1"},
	} {
		if err := env.st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	env.backupAt(t, "b1", "j_hourly", models.StatusCompleted, now.Add(-2*time.Hour), "", 1)
	env.backupAt(t, "b2", "j_hourly", models.StatusFailed, now.Add(-time.Hour), "", 1)
	env.backupAt(t, "b3", "j_daily", models.StatusCompleted, now.Add(-3*time.Hour), "", 1)
	env.backupAt(t, "b4", "j_weekly", models.StatusInProgress, now, "", 0)
	env.backupAt(t, "b5", "j_monthly", models.StatusPruned, now.Add(-48*time.Hour), "", 0)
	return env
}

func jobIDs(jobs []*models.Job) string {
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

func TestFilterJobs(t *testing.T) {
	env := jobListEnv(t)
	on, off := true, false
	for _, tc := range []struct {
		name string
		f    operations.BulkFilter
		want string
	}{
		{"none", operations.BulkFilter{}, "j_daily,j_hourly,j_monthly,j_never,j_weekly"},
		{"enabled", operations.BulkFilter{Enabled: &on}, "j_daily,j_hourly,j_monthly"},
		{"paused", operations.BulkFilter{Enabled: &off}, "j_never,j_weekly"},
		{"connection", operations.BulkFilter{ConnectionID: "c2"}, "j_daily,j_monthly"},
		{"database", operations.BulkFilter{Database: "shop"}, "j_hourly,j_weekly"},
		{"schedule daily", operations.BulkFilter{Schedule: operations.FrequencyDaily}, "j_daily,j_never"},
		{"schedule hourly", operations.BulkFilter{Schedule: operations.FrequencyHourly}, "j_hourly"},
		{"last failed", operations.BulkFilter{LastStatus: operations.LastRunFailed}, "j_hourly"},
		{"last completed (pruned counts)", operations.BulkFilter{LastStatus: operations.LastRunCompleted}, "j_daily,j_monthly"},
		{"last running", operations.BulkFilter{LastStatus: operations.LastRunInProgress}, "j_weekly"},
		{"never ran", operations.BulkFilter{LastStatus: operations.LastRunNever}, "j_never"},
		{"search name, any case", operations.BulkFilter{Q: "NIGHTLY"}, "j_daily"},
		{"search id", operations.BulkFilter{Q: "j_mon"}, "j_monthly"},
		{"combined", operations.BulkFilter{Database: "crm", Enabled: &off, Schedule: operations.FrequencyDaily}, "j_never"},
		{"no match", operations.BulkFilter{Database: "shop", Schedule: operations.FrequencyMonthly}, ""},
	} {
		jobs, err := env.svc.FilterJobs(admin(), tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := jobIDs(jobs); got != tc.want {
			t.Errorf("%s = %s; want %s", tc.name, got, tc.want)
		}
	}
}

func TestFilterJobsValidation(t *testing.T) {
	env := jobListEnv(t)
	for _, f := range []operations.BulkFilter{
		{Schedule: "yearly"},
		{LastStatus: "succeeded"},
		{Status: "failed"},
		{From: "2026-01-01T00:00:00Z"},
		{Q: strings.Repeat("x", 300)},
	} {
		if _, err := env.svc.FilterJobs(admin(), f); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("FilterJobs(%+v) err = %v; want ErrInvalid", f, err)
		}
	}
}

// Multi-database jobs match every database they cover, and their last status is
// their last run's, never one database's backup.
func TestFilterJobsMultiDatabase(t *testing.T) {
	env := jobListEnv(t)
	ctx := admin()
	multi := []*models.Job{
		{ID: "m_list", Name: "Fleet", Database: "orders", CronExpression: "@daily", ConnectionID: "c1",
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"orders", "billing"}}},
		{ID: "m_quiet", Name: "Quiet fleet", Database: "a", CronExpression: "@daily", ConnectionID: "c1",
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}},
	}
	for _, j := range multi {
		if err := env.st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := env.st.SaveJobRun(ctx, &models.JobRun{ID: "run_1", JobID: "m_list", Status: models.JobRunPartial, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	// A completed backup of one database does not make the partial run a success.
	env.backupAt(t, "mb1", "m_list", models.StatusCompleted, now, "", 1)

	for _, tc := range []struct {
		name string
		f    operations.BulkFilter
		want string
	}{
		{"covered database", operations.BulkFilter{Database: "billing"}, "m_list"},
		{"partial run", operations.BulkFilter{LastStatus: operations.LastRunPartial}, "m_list"},
		{"not a success", operations.BulkFilter{LastStatus: operations.LastRunCompleted, Q: "fleet"}, ""},
		{"never ran", operations.BulkFilter{LastStatus: operations.LastRunNever, Q: "fleet"}, "m_quiet"},
	} {
		jobs, err := env.svc.FilterJobs(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := jobIDs(jobs); got != tc.want {
			t.Errorf("%s = %s; want %s", tc.name, got, tc.want)
		}
	}
}

// last_status of multi-database jobs, read in one batch, matches the newest run each
// job's ListJobRuns(1) returns (the per-job lookup it replaced).
func TestFilterJobsLastStatusMatchesPerJobRuns(t *testing.T) {
	env := jobListEnv(t)
	ctx := admin()
	now := time.Now().UTC()
	statuses := []models.JobRunStatus{models.JobRunOK, models.JobRunPartial, models.JobRunFailed, models.JobRunCancelled, models.JobRunRunning}
	for i := range 12 {
		id := fmt.Sprintf("m_%02d", i)
		if err := env.st.SaveJob(ctx, &models.Job{ID: id, Name: id, Database: "a", CronExpression: "@daily", ConnectionID: "c1",
			DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}}); err != nil {
			t.Fatal(err)
		}
		// Jobs 10 and 11 never ran; the others have an older run of another status.
		for k := 0; k < 2 && i < 10; k++ {
			run := &models.JobRun{ID: fmt.Sprintf("run_%02d_%d", i, k), JobID: id, Status: statuses[(i+k)%len(statuses)], StartedAt: now.Add(time.Duration(k) * time.Minute)}
			if err := env.st.SaveJobRun(ctx, run); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, want := range operations.JobLastStatuses {
		jobs, err := env.svc.FilterJobs(ctx, operations.BulkFilter{LastStatus: want, Q: "m_"})
		if err != nil {
			t.Fatal(err)
		}
		var expected []string
		for i := range 12 {
			id := fmt.Sprintf("m_%02d", i)
			runs, err := env.st.ListJobRuns(ctx, id, 1)
			if err != nil {
				t.Fatal(err)
			}
			status := operations.LastRunNever
			if len(runs) == 1 {
				status = map[models.JobRunStatus]string{
					models.JobRunOK: operations.LastRunCompleted, models.JobRunPartial: operations.LastRunPartial,
					models.JobRunFailed: operations.LastRunFailed, models.JobRunCancelled: operations.LastRunCancelled,
					models.JobRunRunning: operations.LastRunInProgress,
				}[runs[0].Status]
			}
			if status == want {
				expected = append(expected, id)
			}
		}
		if got := jobIDs(jobs); got != strings.Join(expected, ",") {
			t.Errorf("last_status %s = %s; per-job runs give %s", want, got, strings.Join(expected, ","))
		}
	}
}

// The bulk job actions select with the same matcher as the jobs list.
func TestBulkJobFilterMatchesTheList(t *testing.T) {
	env := jobListEnv(t)
	off := false
	for _, f := range []operations.BulkFilter{
		{Schedule: operations.FrequencyDaily},
		{LastStatus: operations.LastRunFailed},
		{Enabled: &off, ConnectionID: "c1"},
	} {
		listed, err := env.svc.FilterJobs(admin(), f)
		if err != nil {
			t.Fatal(err)
		}
		res, err := env.svc.Bulk(admin(), operations.BulkJobs, operations.BulkRequest{Action: "delete", Filter: &f, DryRun: true})
		if err != nil {
			t.Fatalf("bulk dry run with %+v: %v", f, err)
		}
		got := slices.Clone(res.ActionableIDs)
		slices.Sort(got)
		if res.Matched != len(listed) || strings.Join(got, ",") != jobIDs(listed) {
			t.Errorf("filter %+v: bulk selected %v (matched %d), list has %s", f, got, res.Matched, jobIDs(listed))
		}
	}
	// Job-only fields are refused for backups.
	if _, err := env.svc.Bulk(admin(), operations.BulkBackups, operations.BulkRequest{Action: "delete", DryRun: true,
		Filter: &operations.BulkFilter{Schedule: operations.FrequencyDaily}}); !errors.Is(err, operations.ErrInvalid) {
		t.Errorf("backups filtered by schedule: err = %v; want ErrInvalid", err)
	}
}
