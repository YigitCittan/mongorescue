package scheduler

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// hookedScheduler is a scheduler whose fake mongodump calls hook with the database
// before it dumps it.
func hookedScheduler(t *testing.T, st store.Store, pub *recordingPublisher, server []string, hook func(db string)) *Scheduler {
	t.Helper()
	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		if hook != nil {
			hook(dumpedDatabase(args))
		}
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	mock := storage.NewMockStorage()
	return NewScheduler(st, backup.NewEngine(mock, "mongodb://localhost:27017", backup.WithRunner(runner)), mock, nil,
		WithPublisher(pub), WithDatabaseLister(func(context.Context, string) ([]string, error) { return slices.Clone(server), nil }))
}

// editJob replaces the stored job's selection and known databases, as a save does.
func editJob(t *testing.T, st store.Store, id string, sel models.DatabaseSelection, known []string) {
	t.Helper()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	job.DatabaseSelection, job.KnownDatabases = sel, known
	if err := st.UpdateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func addedEvents(pub *recordingPublisher) []events.Event {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	var out []events.Event
	for _, e := range pub.got {
		if e.Type == events.JobDatabasesAdded {
			out = append(out, e)
		}
	}
	return out
}

func TestKnownDatabasesFollowTheStoredJob(t *testing.T) {
	server := []string{"prod_a", "prod_b", "dev_x", "x_1"}
	planned := models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}, AutoIncludeNew: true}
	for _, tt := range []struct {
		name      string
		known     []string // known databases when the run starts
		edit      *models.DatabaseSelection
		editKnown []string
		want      []string // known databases afterwards
		added     []string // databases announced as added
	}{
		{name: "unchanged, first run", known: nil, want: []string{"prod_a", "prod_b"}},
		{name: "unchanged, new database", known: []string{"prod_a"}, want: []string{"prod_a", "prod_b"}, added: []string{"prod_b"}},
		{name: "selection changed to unrelated", known: []string{"prod_a"},
			edit: &models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"dev_*"}, AutoIncludeNew: true}, editKnown: []string{"dev_x"},
			want: []string{"dev_x"}},
		{name: "selection widened", known: []string{"prod_a"},
			edit: &models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*", "x_*"}, AutoIncludeNew: true}, editKnown: []string{"x_1"},
			want: []string{"prod_a", "prod_b", "x_1"}, added: []string{"prod_b"}},
		{name: "selection changed, nothing recorded yet", known: []string{"prod_a"},
			edit: &models.DatabaseSelection{Mode: models.SelectionAll, AutoIncludeNew: true}, editKnown: nil, want: nil},
		{name: "auto include switched off", known: []string{"prod_a"},
			edit: &models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"prod_*"}}, editKnown: []string{"prod_a"},
			want: []string{"prod_a", "prod_b"}, added: []string{"prod_b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := storetest.New(t)
			pub := &recordingPublisher{}
			var edited atomic.Bool
			hook := func(string) {
				if tt.edit != nil && edited.CompareAndSwap(false, true) {
					editJob(t, st, "job_k", *tt.edit, tt.editKnown)
				}
			}
			sched := hookedScheduler(t, st, pub, server, hook)
			job := &models.Job{ID: "job_k", CronExpression: "@daily", ConnectionID: "conn", DatabaseSelection: planned, KnownDatabases: tt.known}
			if err := st.SaveJob(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if _, err := sched.runBackupForJob(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetJob(context.Background(), "job_k")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(stored.KnownDatabases, tt.want) || (tt.want == nil) != (stored.KnownDatabases == nil) {
				t.Errorf("known = %#v; want %#v", stored.KnownDatabases, tt.want)
			}
			var got []string
			for _, e := range addedEvents(pub) {
				got = append(got, e.Databases...)
			}
			if !slices.Equal(got, tt.added) {
				t.Errorf("job.databases_added for %v; want %v", got, tt.added)
			}
		})
	}
}

func TestKnownDatabasesAreNeverAnnouncedTwice(t *testing.T) {
	st := storetest.New(t)
	pub := &recordingPublisher{}
	sel := models.DatabaseSelection{Mode: models.SelectionPattern, Include: []string{"app_*"}, AutoIncludeNew: true}
	var once atomic.Bool
	// While the run backs up, a save carries app_b into the known databases (as a
	// concurrent save of the job does): the run must not announce it again.
	hook := func(string) {
		if once.CompareAndSwap(false, true) {
			editJob(t, st, "job_d", sel, []string{"app_a", "app_b"})
		}
	}
	sched := hookedScheduler(t, st, pub, []string{"app_a", "app_b"}, hook)
	job := &models.Job{ID: "job_d", CronExpression: "@daily", ConnectionID: "conn", DatabaseSelection: sel, KnownDatabases: []string{"app_a"}}
	if err := st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.runBackupForJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := addedEvents(pub); len(got) != 0 {
		t.Errorf("job.databases_added = %+v; app_b was already known", got)
	}
	runs, _ := st.ListJobRuns(context.Background(), "job_d", 1)
	if len(runs) != 1 || len(runs[0].AddedDatabases) != 0 {
		t.Errorf("run = %+v; want no added databases", runs)
	}
}

func TestRunProgressIsStoredAfterEveryDatabase(t *testing.T) {
	st := storetest.New(t)
	var seen []models.JobRunDatabase
	hook := func(db string) {
		if db != "b" {
			return
		}
		list, err := st.ListJobRuns(context.Background(), "job_p", 1)
		if err == nil && len(list) == 1 {
			seen = list[0].Databases
		}
	}
	sched := hookedScheduler(t, st, &recordingPublisher{}, []string{"a", "b"}, hook)
	job := &models.Job{ID: "job_p", CronExpression: "@daily", ConnectionID: "conn",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a", "b"}}}
	if err := st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.runBackupForJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Status != models.StatusCompleted || seen[1].Status != models.StatusInProgress {
		t.Errorf("stored run while b ran = %+v; want a completed and b in progress", seen)
	}
}

func TestRestoreTestsDoNotHoldTheJobRun(t *testing.T) {
	st := storetest.New(t)
	sched := hookedScheduler(t, st, &recordingPublisher{}, []string{"a"}, nil)
	var activeDuringTests string
	called := false
	sched.afterRun = func(_ context.Context, job *models.Job, _ []*models.BackupRecord) {
		called = true
		activeDuringTests = sched.ActiveJobRun(job.ID)
	}
	job := &models.Job{ID: "job_rt", CronExpression: "@daily", ConnectionID: "conn",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionAll}}
	if err := st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.runBackupForJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !called || activeDuringTests != "" {
		t.Errorf("restore tests ran %v while run %q was still active; the next run must not be blocked", called, activeDuringTests)
	}
}

func TestLockErrorsOtherThanBusyAreNamed(t *testing.T) {
	f := newMultiFixture(t, "a")
	f.sched.guard = func(string, string) (func(), error) { return nil, io.ErrUnexpectedEOF }
	f.job(t, "job_lock", models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"a"}}, nil)
	_, _ = f.sched.runBackupForJob(context.Background(), mustJob(t, f.store, "job_lock"))
	runs, _ := f.store.ListJobRuns(context.Background(), "job_lock", 1)
	if len(runs) != 1 || !strings.Contains(runs[0].Databases[0].Error, "could not be taken") ||
		strings.Contains(runs[0].Databases[0].Error, "still running") {
		t.Errorf("run = %+v", runs)
	}
}

func TestStartJobRunReturnsAtOnce(t *testing.T) {
	f := newMultiFixture(t, "a", "b")
	f.job(t, "job_async", models.DatabaseSelection{Mode: models.SelectionAll}, nil)
	var fn func(context.Context)
	run, err := f.sched.StartJobRun(context.Background(), "job_async", models.TriggerOnDemand, func(f func(context.Context)) error {
		fn = f
		return nil
	})
	if err != nil || run.Status != models.JobRunRunning || len(run.Databases) != 0 {
		t.Fatalf("StartJobRun = %+v, %v; want a running run before any planning", run, err)
	}
	if _, err := f.sched.StartJobRun(context.Background(), "job_async", models.TriggerOnDemand, func(func(context.Context)) error { return nil }); err == nil {
		t.Fatal("a second run while one is pending must be refused")
	}
	fn(context.Background())
	stored, _ := f.store.GetJobRun(context.Background(), run.ID)
	if stored == nil || stored.Status != models.JobRunOK || len(stored.Databases) != 2 || f.sched.ActiveJobRun("job_async") != "" {
		t.Errorf("run after the background work = %+v", stored)
	}
	f.job(t, "job_single", models.DatabaseSelection{Mode: models.SelectionSingle, Databases: []string{"a"}}, nil)
	if _, err := f.sched.StartJobRun(context.Background(), "job_single", models.TriggerOnDemand, nil); err == nil {
		t.Error("a single-database job must be refused")
	}
}
