package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runlog"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// dumpedDatabase returns the --db argument of a mongodump invocation.
func dumpedDatabase(args []string) string {
	for _, a := range args {
		if db, ok := strings.CutPrefix(a, "--db="); ok {
			return db
		}
	}
	return ""
}

// multiFixture is a scheduler with a fake database listing and a fake mongodump
// that fails the databases in failing.
type multiFixture struct {
	sched   *Scheduler
	store   store.Store
	pub     *recordingPublisher
	reg     *runs.Registry
	mu      sync.Mutex
	server  []string
	failing map[string]bool
	dumped  []string
}

func newMultiFixture(t *testing.T, server ...string) *multiFixture {
	t.Helper()
	f := &multiFixture{store: storetest.New(t), pub: &recordingPublisher{}, server: server, failing: map[string]bool{}}
	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		db := dumpedDatabase(args)
		f.mu.Lock()
		f.dumped = append(f.dumped, db)
		fail := f.failing[db]
		f.mu.Unlock()
		wait := func() error { return nil }
		if fail {
			wait = func() error { return errors.New("exit status 1") }
		}
		return io.NopCloser(bytes.NewReader([]byte("archive of " + db))), strings.NewReader(""), wait, nil
	}
	mock := storage.NewMockStorage()
	f.reg = runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	f.sched = NewScheduler(f.store, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(runner)), mock, nil,
		WithPublisher(f.pub), WithRunRegistry(f.reg),
		WithDatabaseLister(func(context.Context, string) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return slices.Clone(f.server), nil
		}))
	return f
}

func (f *multiFixture) job(t *testing.T, id string, sel models.DatabaseSelection, known []string) *models.Job {
	t.Helper()
	job := &models.Job{ID: id, Name: id, CronExpression: "@daily", Enabled: true, ConnectionID: "conn", DatabaseSelection: sel, KnownDatabases: known}
	if err := f.store.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *multiFixture) events() []events.Event {
	f.pub.mu.Lock()
	defer f.pub.mu.Unlock()
	return slices.Clone(f.pub.got)
}

// summaries returns the events notifications would deliver for backups: those not
// marked InRun.
func summaries(list []events.Event) []events.Event {
	var out []events.Event
	for _, e := range list {
		if !e.InRun && (e.Type == events.BackupSucceeded || e.Type == events.BackupFailed || e.Type == events.BackupCancelled) {
			out = append(out, e)
		}
	}
	return out
}

func TestListModeRecordsMissingDatabasesAndRunsTheOthers(t *testing.T) {
	f := newMultiFixture(t, "admin", "shop", "billing")
	f.job(t, "job_list", models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"shop", "gone", "billing"}}, nil)
	ctx := context.Background()

	if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_list")); !errors.Is(err, ErrRunFailed) {
		t.Fatalf("run error = %v; want ErrRunFailed (partial)", err)
	}
	runList, err := f.store.ListJobRuns(ctx, "job_list", 10)
	if err != nil || len(runList) != 1 {
		t.Fatalf("job runs = %+v, %v", runList, err)
	}
	run := runList[0]
	if run.Status != models.JobRunPartial || len(run.Databases) != 3 {
		t.Fatalf("run = %+v; want partial over 3 databases", run)
	}
	byName := map[string]models.JobRunDatabase{}
	for _, d := range run.Databases {
		byName[d.Database] = d
	}
	if d := byName["gone"]; d.Status != models.StatusFailed || d.Error != models.ErrorDatabaseNotFound || d.BackupID != "" {
		t.Errorf("missing database = %+v; want failed with %q and no backup", d, models.ErrorDatabaseNotFound)
	}
	for _, db := range []string{"shop", "billing"} {
		d := byName[db]
		rec, getErr := f.store.GetBackupRecord(ctx, d.BackupID)
		if getErr != nil || rec.Status != models.StatusCompleted || rec.RunID != run.ID || rec.Database != db || rec.JobID != "job_list" ||
			!strings.HasPrefix(rec.StorageKey, db+"/") {
			t.Errorf("backup of %s = %+v, %v", db, rec, getErr)
		}
	}
	page, err := f.store.QueryBackupRecords(ctx, store.BackupFilter{RunID: run.ID})
	if err != nil || page.Total != 2 {
		t.Errorf("backups of the run = %+v, %v; want 2", page, err)
	}
	// One summary for the run, listing the failed database.
	sums := summaries(f.events())
	if len(sums) != 1 || sums[0].Type != events.BackupFailed || sums[0].Run == nil || !sums[0].Run.Multi ||
		sums[0].Run.Status != string(models.JobRunPartial) || !slices.Equal(sums[0].Run.FailedDatabases, []string{"gone"}) {
		t.Fatalf("summary events = %+v; want one partial backup.failed listing gone", sums)
	}
}

func mustJob(t *testing.T, st store.Store, id string) *models.Job {
	t.Helper()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestRunStatusFromDatabaseOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		failing []string
		want    models.JobRunStatus
	}{
		{"all ok", nil, models.JobRunOK},
		{"some fail", []string{"b"}, models.JobRunPartial},
		{"all fail", []string{"a", "b"}, models.JobRunFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newMultiFixture(t, "a", "b", "local")
			for _, db := range tt.failing {
				f.failing[db] = true
			}
			f.job(t, "job_all", models.DatabaseSelection{Mode: models.SelectionAll}, nil)
			plan, err := f.sched.PrepareJobRun(context.Background(), "job_all", models.TriggerOnDemand)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sched.BeginJobRun(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			run, _ := f.sched.ExecuteJobRun(context.Background(), plan)
			if run.Status != tt.want {
				t.Fatalf("status = %s; want %s (%+v)", run.Status, tt.want, run.Databases)
			}
			if got := f.dumped; !slices.Equal(got, []string{"a", "b"}) {
				t.Errorf("dumped %v; the system database local must never be backed up", got)
			}
			if sums := summaries(f.events()); len(sums) != 1 {
				t.Errorf("%d summary events; want exactly one per run", len(sums))
			}
			if f.sched.ActiveJobRun("job_all") != "" {
				t.Error("the job run is still marked active")
			}
		})
	}
}

func TestPatternWithoutAutoIncludeFreezesKnownDatabases(t *testing.T) {
	f := newMultiFixture(t, "prod_a", "prod_b", "dev_a")
	f.job(t, "job_p", models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}}, nil)
	ctx := context.Background()

	if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_p")); err != nil {
		t.Fatal(err)
	}
	if job := mustJob(t, f.store, "job_p"); !slices.Equal(job.KnownDatabases, []string{"prod_a", "prod_b"}) {
		t.Fatalf("known databases after the first run = %v", job.KnownDatabases)
	}

	f.mu.Lock()
	f.server = append(f.server, "prod_c")
	f.dumped = nil
	f.mu.Unlock()
	if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_p")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.dumped, []string{"prod_a", "prod_b"}) {
		t.Errorf("second run dumped %v; a new database must not be backed up without auto_include_new", f.dumped)
	}
	runList, _ := f.store.ListJobRuns(ctx, "job_p", 1)
	if len(runList) != 1 || !slices.Equal(runList[0].NewDatabases, []string{"prod_c"}) || runList[0].Status != models.JobRunOK {
		t.Errorf("latest run = %+v; want ok with prod_c reported as new", runList)
	}
	if job := mustJob(t, f.store, "job_p"); !slices.Equal(job.KnownDatabases, []string{"prod_a", "prod_b"}) {
		t.Errorf("known databases = %v; a new database must stay unknown", job.KnownDatabases)
	}
	for _, e := range f.events() {
		if e.Type == events.JobDatabasesAdded {
			t.Errorf("job.databases_added published without auto_include_new: %+v", e)
		}
	}
}

func TestPatternWithAutoIncludeAddsNewDatabasesOnce(t *testing.T) {
	f := newMultiFixture(t, "prod_a", "dev_a")
	f.job(t, "job_auto", models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}, AutoIncludeNew: true}, []string{"prod_a"})
	ctx := context.Background()

	f.server = append(f.server, "prod_b")
	if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_auto")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.dumped, []string{"prod_a", "prod_b"}) {
		t.Errorf("dumped %v; want the new prod_b included", f.dumped)
	}
	if job := mustJob(t, f.store, "job_auto"); !slices.Equal(job.KnownDatabases, []string{"prod_a", "prod_b"}) {
		t.Errorf("known = %v", job.KnownDatabases)
	}
	if _, err := f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_auto")); err != nil {
		t.Fatal(err)
	}
	var added []events.Event
	for _, e := range f.events() {
		if e.Type == events.JobDatabasesAdded {
			added = append(added, e)
		}
	}
	if len(added) != 1 || !slices.Equal(added[0].Databases, []string{"prod_b"}) || added[0].JobID != "job_auto" {
		t.Fatalf("job.databases_added events = %+v; want one for prod_b, the first time only", added)
	}
}

func TestCancellingOneDatabaseStopsTheWholeRun(t *testing.T) {
	metaStore := storetest.New(t)
	var started atomic.Int32
	blocking := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		started.Add(1)
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, eofReader{}, func() error { <-ctx.Done(); return errors.New("signal: killed") }, nil
	}
	mock := storage.NewMockStorage()
	reg := runs.NewRegistry(runs.WithLogs(runlog.NewDir(t.TempDir())))
	pub := &recordingPublisher{}
	sched := NewScheduler(metaStore, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(blocking)), mock, nil,
		WithRunRegistry(reg), WithPublisher(pub),
		WithDatabaseLister(func(context.Context, string) ([]string, error) { return []string{"a", "b", "c"}, nil }))
	job := &models.Job{ID: "job_c", CronExpression: "@daily", Enabled: true, ConnectionID: "conn",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	plan, err := sched.PrepareJobRun(context.Background(), job.ID, models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.BeginJobRun(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	// Queued databases are tracked before the run starts.
	if got := len(reg.JobRuns(job.ID)); got != 3 {
		t.Fatalf("%d tracked runs; want all 3 databases", got)
	}
	if _, err := sched.PrepareJobRun(context.Background(), job.ID, models.TriggerOnDemand); !errors.Is(err, runs.ErrBusy) {
		t.Fatalf("second run while one is going: %v; want ErrBusy", err)
	}
	done := make(chan *models.JobRun, 1)
	go func() {
		run, _ := sched.ExecuteJobRun(context.Background(), plan)
		done <- run
	}()
	for deadline := time.Now().Add(5 * time.Second); started.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first database never started")
		}
	}
	if err := reg.Cancel(plan.Records[0].ID, runs.Cancellation{By: "alice", Kind: runs.ActorUser}); err != nil {
		t.Fatal(err)
	}
	var run *models.JobRun
	select {
	case run = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop")
	}
	if n := started.Load(); n != 1 {
		t.Errorf("mongodump started %d times; the waiting databases must not start after the cancel", n)
	}
	if run.Status != models.JobRunCancelled {
		t.Errorf("run status = %s; want cancelled", run.Status)
	}
	for _, rec := range plan.Records {
		stored, err := metaStore.GetBackupRecord(context.Background(), rec.ID)
		if err != nil || stored.Status != models.StatusCancelled || stored.CancelledBy != "alice" {
			t.Errorf("backup of %s = %+v, %v; want cancelled by alice", rec.Database, stored, err)
		}
	}
	if len(reg.JobRuns(job.ID)) != 0 {
		t.Error("cancelled runs are still tracked")
	}
	if sums := summaries(pub.got); len(sums) != 1 || sums[0].Type != events.BackupCancelled {
		t.Errorf("summary events = %+v; want one backup.cancelled", sums)
	}
}

func TestParallelismBoundsConcurrentDatabases(t *testing.T) {
	metaStore := storetest.New(t)
	var running, peak atomic.Int32
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	mock := storage.NewMockStorage()
	sched := NewScheduler(metaStore, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(runner)), mock, nil,
		WithDatabaseLister(func(context.Context, string) ([]string, error) { return []string{"a", "b", "c", "d", "e"}, nil }))
	job := &models.Job{ID: "job_par", CronExpression: "@daily", Enabled: true, ConnectionID: "conn", Parallelism: 2,
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.TriggerJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p < 1 || p > 2 {
		t.Errorf("peak concurrency %d; want at most the job's parallelism 2", p)
	}
}

func TestBusyDatabaseFailsOnlyThatDatabase(t *testing.T) {
	f := newMultiFixture(t, "a", "b")
	locks := runs.NewManager(nil)
	f.sched.guard = func(connectionID, database string) (func(), error) {
		return locks.Acquire(runs.BackupKey(connectionID, database))
	}
	release, err := locks.Acquire(runs.BackupKey("conn", "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.job(t, "job_busy", models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}, nil)
	_, _ = f.sched.runBackupForJob(context.Background(), mustJob(t, f.store, "job_busy"))
	if !slices.Equal(f.dumped, []string{"a"}) {
		t.Errorf("dumped %v; the database whose lock is held must be skipped", f.dumped)
	}
	runList, _ := f.store.ListJobRuns(context.Background(), "job_busy", 1)
	if len(runList) != 1 || runList[0].Status != models.JobRunPartial {
		t.Fatalf("run = %+v; want partial", runList)
	}
}

func TestPreviewEqualsRunResolution(t *testing.T) {
	f := newMultiFixture(t, "admin", "prod_a", "prod_b", "prod_tmp1", "dev")
	sel := models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}, Exclude: []string{"prod_tmp?"}, Databases: []string{"dev"}}
	job := f.job(t, "job_prev", sel, []string{"prod_a"})
	res, err := f.sched.ResolveJobDatabases(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.sched.PrepareJobRun(context.Background(), job.ID, models.TriggerOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, rec := range plan.Records {
		planned = append(planned, rec.Database)
	}
	slices.Sort(planned)
	if !slices.Equal(res.Included, planned) || !slices.Equal(res.Included, []string{"dev", "prod_a"}) {
		t.Fatalf("preview %v, run plans %v; want both [dev prod_a]", res.Included, planned)
	}
	if !slices.Equal(res.New, []string{"prod_b"}) || !slices.Equal(plan.Run.NewDatabases, res.New) {
		t.Errorf("new: preview %v, run %v; want [prod_b]", res.New, plan.Run.NewDatabases)
	}
}

func TestUnlistableConnectionFailsTheScheduledRun(t *testing.T) {
	metaStore := storetest.New(t)
	pub := &recordingPublisher{}
	mock := storage.NewMockStorage()
	sched := NewScheduler(metaStore, backup.NewEngine(mock, "mongodb://localhost:27017"), mock, nil, WithPublisher(pub),
		WithDatabaseLister(func(context.Context, string) ([]string, error) { return nil, errors.New("connection refused") }))
	job := &models.Job{ID: "job_down", CronExpression: "@daily", Enabled: true, ConnectionID: "conn",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.runBackupForJob(context.Background(), job); !errors.Is(err, ErrDatabaseListing) {
		t.Fatalf("err = %v; want ErrDatabaseListing", err)
	}
	runList, _ := metaStore.ListJobRuns(context.Background(), job.ID, 1)
	if len(runList) != 1 || runList[0].Status != models.JobRunFailed || !strings.Contains(runList[0].Error, "connection refused") {
		t.Fatalf("run = %+v; want failed with the listing error", runList)
	}
	if sums := summaries(pub.got); len(sums) != 1 || sums[0].Type != events.BackupFailed {
		t.Errorf("summaries = %+v; want one backup.failed", sums)
	}
	if _, err := sched.PrepareJobRun(context.Background(), job.ID, models.TriggerOnDemand); !errors.Is(err, ErrDatabaseListing) {
		t.Errorf("on-demand prepare: %v; want ErrDatabaseListing", err)
	}
}

func TestRetentionIsPerDatabase(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rec := func(id, db string, age time.Duration, verified bool) *models.BackupRecord {
		r := &models.BackupRecord{ID: id, JobID: "job", Database: db, Status: models.StatusCompleted,
			Trigger: models.TriggerScheduled, StartedAt: now.Add(-age)}
		if verified {
			r.Verification = models.VerificationOK
		}
		return r
	}
	day := 24 * time.Hour
	records := []*models.BackupRecord{
		// shop ran every day; billing failed for the last 5 days, so its newest good
		// backup is old.
		rec("shop_1", "shop", 1*day, false), rec("shop_2", "shop", 2*day, false), rec("shop_3", "shop", 3*day, false),
		rec("billing_6", "billing", 6*day, false), rec("billing_7", "billing", 7*day, true), rec("billing_8", "billing", 8*day, false),
	}
	plan := PlanRetention(now, 2, 1, records)
	var deleted []string
	for _, d := range plan.Delete {
		deleted = append(deleted, d.Backup.ID)
	}
	slices.Sort(deleted)
	// Per database: the newest backup is the floor (billing keeps billing_6 although
	// it is older than 2 days), billing_7 is billing's newest verified one.
	if want := []string{"billing_8", "shop_2", "shop_3"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted %v; want %v", deleted, want)
	}
	if len(plan.Protected) != 1 || plan.Protected[0].BackupID != "billing_7" || plan.Protected[0].Reason != ProtectedLastVerified {
		t.Errorf("protected = %+v; want billing_7 as last verified", plan.Protected)
	}
	if len(plan.Databases) != 2 || plan.Databases[0].Database != "billing" || plan.Databases[0].LastGood != "billing_6" ||
		plan.Databases[0].Kept != 2 || plan.Databases[1].Database != "shop" || plan.Databases[1].Delete != 2 || plan.Databases[1].Kept != 1 {
		t.Errorf("per-database breakdown = %+v", plan.Databases)
	}
	if plan.Considered != 6 {
		t.Errorf("considered = %d; want 6", plan.Considered)
	}
	for i := 1; i < len(plan.Delete); i++ {
		if plan.Delete[i].Backup.StartedAt.Before(plan.Delete[i-1].Backup.StartedAt) {
			t.Errorf("deletions are not oldest first: %v", deleted)
		}
	}
}

func TestScheduledMultiRunPrunesEachDatabase(t *testing.T) {
	f := newMultiFixture(t, "a", "b")
	f.failing["b"] = true
	ctx := context.Background()
	f.job(t, "job_ret", models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}, nil)
	job := mustJob(t, f.store, "job_ret")
	job.RetentionCount = 1
	if err := f.store.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	for _, r := range []*models.BackupRecord{
		{ID: "old_a", JobID: "job_ret", Database: "a", ConnectionID: "conn", Status: models.StatusCompleted, Trigger: models.TriggerScheduled, StartedAt: old, StorageKey: "a/old"},
		{ID: "old_b", JobID: "job_ret", Database: "b", ConnectionID: "conn", Status: models.StatusCompleted, Trigger: models.TriggerScheduled, StartedAt: old, StorageKey: "b/old"},
	} {
		if err := f.store.SaveBackupRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = f.sched.runBackupForJob(ctx, mustJob(t, f.store, "job_ret"))
	if a, _ := f.store.GetBackupRecord(ctx, "old_a"); a == nil || a.Status != models.StatusPruned {
		t.Errorf("old_a = %+v; want pruned after a's new backup", a)
	}
	if b, _ := f.store.GetBackupRecord(ctx, "old_b"); b == nil || b.Status != models.StatusCompleted {
		t.Errorf("old_b = %+v; b failed today and must keep its last good backup", b)
	}
}
