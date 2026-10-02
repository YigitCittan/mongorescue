package operations_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// multiEnv is an operations service over a real scheduler whose connection lists the
// databases in server.
type multiEnv struct {
	svc    *operations.Service
	st     store.Store
	sched  *scheduler.Scheduler
	mu     sync.Mutex
	server []string
}

func newMultiEnv(t *testing.T, server ...string) *multiEnv {
	t.Helper()
	e := &multiEnv{st: storetest.New(t), server: server}
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	mock := storage.NewMockStorage()
	engine := backup.NewEngine(mock, "", backup.WithRunner(runner))
	manager := runs.NewManager(nil)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	registry := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	conns := fakeConnections{"conn_a": {ID: "conn_a", Name: "primary", URI: "mongodb://u:pw@db.internal/"}}
	e.sched = scheduler.NewScheduler(e.st, engine, mock, nil, scheduler.WithConnectionResolver(conns), scheduler.WithRunRegistry(registry),
		scheduler.WithBackupGuard(func(connectionID, database string) (func(), error) {
			return manager.Acquire(runs.BackupKey(connectionID, database))
		}),
		scheduler.WithDatabaseLister(func(context.Context, string) ([]string, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return slices.Clone(e.server), nil
		}))
	e.svc = operations.New(operations.Config{
		Store: e.st, Backup: engine, Restore: restore.NewEngine(mock, ""), Runs: manager, Registry: registry,
		Jobs: e.sched, Scheduler: e.sched, Connections: conns,
	})
	return e
}

func (e *multiEnv) create(t *testing.T, job *models.Job) *models.Job {
	t.Helper()
	if job.ConnectionID == "" {
		job.ConnectionID = "conn_a"
	}
	if err := e.svc.ValidateJob(context.Background(), job); err != nil {
		t.Fatalf("ValidateJob: %v", err)
	}
	if err := e.st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestValidateJobSelections(t *testing.T) {
	e := newMultiEnv(t, "admin", "prod_a", "prod_b", "dev")
	ctx := context.Background()

	legacy := &models.Job{ID: "job_legacy", Database: "shop", ConnectionID: "conn_a"}
	if err := e.svc.ValidateJob(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if sel := legacy.DatabaseSelection; sel.Mode != models.SelectionSingle || !slices.Equal(sel.Databases, []string{"shop"}) ||
		legacy.Database != "shop" || legacy.Parallelism != 1 {
		t.Errorf("a job sent with database only = %+v", legacy)
	}

	pattern := &models.Job{ID: "job_p", Database: "ignored", ConnectionID: "conn_a",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}}}
	if err := e.svc.ValidateJob(ctx, pattern); err != nil {
		t.Fatal(err)
	}
	if pattern.Database != "" || !slices.Equal(pattern.KnownDatabases, []string{"prod_a", "prod_b"}) {
		t.Errorf("pattern job = database %q, known %v; want no database and the matches recorded", pattern.Database, pattern.KnownDatabases)
	}

	for name, bad := range map[string]*models.Job{
		"no database":     {ID: "j1", ConnectionID: "conn_a"},
		"collections":     {ID: "j2", ConnectionID: "conn_a", Collections: []string{"orders"}, DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}},
		"parallelism":     {ID: "j3", ConnectionID: "conn_a", Parallelism: 5, DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}},
		"system database": {ID: "j4", ConnectionID: "conn_a", DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"config"}}},
		"bad glob":        {ID: "j5", ConnectionID: "conn_a", DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"a.b*"}}},
		"bad mode":        {ID: "j6", ConnectionID: "conn_a", DatabaseSelection: models.DatabaseSelection{Mode: "everything"}},
	} {
		if err := e.svc.ValidateJob(ctx, bad); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
}

func TestUpdateJobKeepsSelectionForOldClients(t *testing.T) {
	e := newMultiEnv(t, "a", "b")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_m", Name: "m", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}})

	// A client that only knows single-database jobs (such as an old dashboard
	// pausing the job) sends no selection and no database.
	enabled := false
	job, err := e.svc.UpdateJob(ctx, "job_m", operations.JobUpdate{Name: "m", CronExpression: "@daily", ConnectionID: "conn_a", Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if job.DatabaseSelection.Mode != models.SelectionList || !slices.Equal(job.DatabaseSelection.Databases, []string{"a", "b"}) {
		t.Errorf("selection after an update without one = %+v", job.DatabaseSelection)
	}
	// database makes it a single-database job, as it always did.
	job, err = e.svc.UpdateJob(ctx, "job_m", operations.JobUpdate{Name: "m", CronExpression: "@daily", ConnectionID: "conn_a", Database: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if job.MultiDatabase() || job.Database != "a" {
		t.Errorf("after database=a: %+v", job)
	}
	sel := &models.DatabaseSelection{Mode: models.SelectionAll, Exclude: []string{"b"}}
	par := 3
	job, err = e.svc.UpdateJob(ctx, "job_m", operations.JobUpdate{Name: "m", CronExpression: "@daily", ConnectionID: "conn_a",
		DatabaseSelection: sel, Parallelism: &par})
	if err != nil {
		t.Fatal(err)
	}
	if job.DatabaseSelection.Mode != models.SelectionAll || job.Parallelism != 3 || !slices.Equal(job.KnownDatabases, []string{"a"}) {
		t.Errorf("after the all selection: %+v", job)
	}
}

func TestPreviewJobDatabases(t *testing.T) {
	e := newMultiEnv(t, "admin", "prod_a", "prod_b", "dev")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_p", Name: "p", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}}})
	e.mu.Lock()
	e.server = append(e.server, "prod_c")
	e.mu.Unlock()

	p, err := e.svc.PreviewJobDatabases(ctx, "job_p", operations.DatabasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Included, []string{"prod_a", "prod_b"}) || !slices.Equal(p.New, []string{"prod_c"}) {
		t.Errorf("preview of the stored job = %+v", p.DatabaseResolution)
	}
	// The run resolves exactly what the preview showed.
	plan, err := e.sched.PrepareJobRun(ctx, "job_p", models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, r := range plan.Records {
		planned = append(planned, r.Database)
	}
	if !slices.Equal(planned, p.Included) {
		t.Errorf("run plans %v; preview showed %v", planned, p.Included)
	}

	// A changed selection is previewed as saving it would record it.
	changed, err := e.svc.PreviewJobDatabases(ctx, "job_p", operations.DatabasePreviewRequest{
		Selection: &models.DatabaseSelection{Mode: models.SelectionAll, Exclude: []string{"dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed.Included, []string{"prod_a", "prod_b", "prod_c"}) || len(changed.New) != 0 {
		t.Errorf("preview of a changed selection = %+v", changed.DatabaseResolution)
	}

	// An unsaved job.
	draft, err := e.svc.PreviewJobDatabases(ctx, "", operations.DatabasePreviewRequest{ConnectionID: "conn_a",
		Selection: &models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"dev", "nope"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(draft.Included, []string{"dev"}) || !slices.Equal(draft.Missing, []string{"nope"}) {
		t.Errorf("draft preview = %+v", draft.DatabaseResolution)
	}
	if _, err := e.svc.PreviewJobDatabases(ctx, "", operations.DatabasePreviewRequest{ConnectionID: "conn_a"}); !errors.Is(err, operations.ErrInvalid) {
		t.Errorf("draft without a selection: %v; want ErrInvalid", err)
	}
	if _, err := e.svc.PreviewJobDatabases(ctx, "missing", operations.DatabasePreviewRequest{}); !errors.Is(err, operations.ErrNotFound) {
		t.Errorf("unknown job: %v; want ErrNotFound", err)
	}
}

func TestRunMultiDatabaseJobOnDemand(t *testing.T) {
	e := newMultiEnv(t, "a", "b", "c")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_run", Name: "run", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "c"}}})

	if _, err := e.svc.CancelJobRun(ctx, "job_run", ""); !errors.Is(err, operations.ErrNotRunning) {
		t.Errorf("cancel without a run: %v; want ErrNotRunning", err)
	}
	started, err := e.svc.RunJob(ctx, "job_run", models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	// A multi-database job answers at once with its run; the databases follow.
	run := started.Run
	if started.Backup != nil || run == nil || run.Status != models.JobRunRunning || run.Trigger != models.TriggerOnDemand || run.JobID != "job_run" {
		t.Fatalf("started = %+v", started)
	}
	awaitRun := func(jobID string) *models.JobRun {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			list, listErr := e.svc.ListJobRuns(ctx, jobID, 10)
			if listErr == nil && len(list) == 1 && list[0].Status != models.JobRunRunning {
				return list[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("the run of %s did not finish: %+v, %v", jobID, list, listErr)
			}
		}
	}
	done := awaitRun("job_run")
	if done.Status != models.JobRunOK || done.ID != run.ID || len(done.Databases) != 2 {
		t.Errorf("run = %+v", done)
	}
	page, err := e.svc.QueryBackups(ctx, operations.BackupFilter{RunID: run.ID})
	if err != nil || page.Total != 2 {
		t.Errorf("backups of the run = %+v, %v", page, err)
	}
	// Planning failures are recorded in the run: databases that are all gone.
	e.create(t, &models.Job{ID: "job_none", Name: "none", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"gone"}}})
	if _, err := e.svc.RunJob(ctx, "job_none", models.TriggerOnDemand); err != nil {
		t.Fatalf("run of a job whose databases are gone: %v; want the run recorded", err)
	}
	if none := awaitRun("job_none"); none.Status != models.JobRunFailed || none.Databases[0].Error != models.ErrorDatabaseNotFound {
		t.Errorf("run of job_none = %+v", none)
	}
	// And a connection with no database: an all job matches nothing.
	e.mu.Lock()
	e.server = nil
	e.mu.Unlock()
	e.create(t, &models.Job{ID: "job_empty", Name: "empty", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}})
	if _, err := e.svc.RunJob(ctx, "job_empty", models.TriggerOnDemand); err != nil {
		t.Fatal(err)
	}
	if empty := awaitRun("job_empty"); empty.Status != models.JobRunFailed || empty.Error == "" {
		t.Errorf("run of job_empty = %+v", empty)
	}
}

func TestRetentionPreviewBreaksDownPerDatabase(t *testing.T) {
	e := newMultiEnv(t, "a", "b")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_ret", Name: "ret", CronExpression: "@daily", RetentionCount: 1,
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}})
	job, _ := e.st.GetJob(ctx, "job_ret")
	old := time.Now().Add(-72 * time.Hour)
	for i, db := range []string{"a", "a", "b"} {
		r := &models.BackupRecord{ID: "bkp_" + db + string(rune('0'+i)), JobID: job.ID, Database: db, ConnectionID: "conn_a",
			StorageTargetID: job.StorageTargetID, Status: models.StatusCompleted, Trigger: models.TriggerScheduled,
			StartedAt: old.Add(time.Duration(i) * time.Hour)}
		if err := e.st.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	p, err := e.svc.RetentionPreview(ctx, "job_ret", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Delete) != 1 || p.Delete[0].Backup.ID != "bkp_a0" || len(p.Databases) != 2 || p.Databases[1].Kept != 1 {
		t.Errorf("preview = %+v", p)
	}
}

func TestStatusReportsTheStalestDatabase(t *testing.T) {
	e := newMultiEnv(t, "a", "b")
	ctx := context.Background()
	e.create(t, &models.Job{ID: "job_st", Name: "st", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b", "c"}}})
	now := time.Now().UTC()
	for _, r := range []struct {
		id, db string
		age    time.Duration
		status models.BackupStatus
	}{
		{"a_new", "a", time.Hour, models.StatusCompleted},
		{"b_old", "b", 72 * time.Hour, models.StatusCompleted},
		{"b_fail", "b", time.Hour, models.StatusFailed},
		{"c_fail", "c", time.Hour, models.StatusFailed},
	} {
		at := now.Add(-r.age)
		if err := e.st.SaveBackupRecord(ctx, &models.BackupRecord{ID: r.id, JobID: "job_st", Database: r.db, Status: r.status,
			StartedAt: at, CompletedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	jobStatus := func() operations.JobStatus {
		t.Helper()
		st, err := e.svc.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, js := range st.JobStatus {
			if js.ID == "job_st" {
				return js
			}
		}
		t.Fatal("job missing from the status")
		return operations.JobStatus{}
	}
	// c never succeeded: the job has no last success, and says which database.
	js := jobStatus()
	if js.LastSuccessAt != nil || js.StalestDatabase != "c" || len(js.Databases) != 3 || js.Databases[0].LastSuccessBackupID != "a_new" {
		t.Fatalf("status = %+v", js)
	}
	// Without c, b (failing daily, last good three days ago) is the stalest.
	e.create(t, &models.Job{ID: "job_st2", Name: "st2", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}})
	for _, b := range []string{"a_new", "b_old"} {
		rec, _ := e.st.GetBackupRecord(ctx, b)
		rec.ID, rec.JobID = rec.ID+"_2", "job_st2"
		if err := e.st.SaveBackupRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := e.svc.Status(ctx)
	for _, js := range st.JobStatus {
		if js.ID == "job_st2" && (js.LastSuccessAt == nil || js.LastSuccessBackupID != "b_old_2" || js.StalestDatabase != "b") {
			t.Errorf("status of job_st2 = %+v; want b's three-day-old backup", js)
		}
	}
}

func TestBulkJobFilterCoversSelections(t *testing.T) {
	e := newMultiEnv(t, "prod_a", "shop")
	ctx := asUser("admin", "admin")
	e.create(t, &models.Job{ID: "job_list", Name: "list", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"shop", "crm"}}})
	e.create(t, &models.Job{ID: "job_pat", Name: "pat", CronExpression: "@daily",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}}})
	e.create(t, &models.Job{ID: "job_one", Name: "one", CronExpression: "@daily", Database: "other"})
	matched := func(f operations.BulkFilter) int {
		t.Helper()
		res, err := e.svc.Bulk(ctx, operations.BulkJobs, operations.BulkRequest{Action: operations.BulkDisable, Filter: &f, DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Matched
	}
	for _, tt := range []struct {
		f    operations.BulkFilter
		want int
	}{
		{operations.BulkFilter{Database: "crm"}, 1},     // listed
		{operations.BulkFilter{Database: "prod_zz"}, 1}, // the pattern matches it
		{operations.BulkFilter{Database: "prod_a"}, 1},  // known
		{operations.BulkFilter{Database: "other"}, 1},
		{operations.BulkFilter{Database: "admin"}, 0},
		{operations.BulkFilter{Q: "prod_"}, 1}, // the pattern's text
		{operations.BulkFilter{Q: "crm"}, 1},
	} {
		if got := matched(tt.f); got != tt.want {
			t.Errorf("filter %+v matched %d; want %d", tt.f, got, tt.want)
		}
	}
}
