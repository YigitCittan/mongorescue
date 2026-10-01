package integrity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// restoreTestFixture is a fixture with job_1 (restore test weekly) and its backup
// bkp_1 whose manifest has orders (10 to 12 documents, two indexes) and users.
func restoreTestFixture(t *testing.T) (*fixture, *models.Job, *models.BackupRecord) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	job := &models.Job{ID: "job_1", Name: "nightly", Database: "shop", CronExpression: "@daily", ConnectionID: "conn_src",
		RestoreTest: &models.RestoreTestPolicy{Enabled: true, Frequency: models.RestoreTestWeekly}}
	if err := f.st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	manifest := &models.Manifest{CapturedAt: f.now, Collections: []models.CollectionManifest{
		{Name: "orders", DocumentsMin: 10, DocumentsMax: 12, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1", Unique: true}}},
		{Name: "users", DocumentsMin: 3, DocumentsMax: 3, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
	}}
	rec := f.putBackup(t, "bkp_1", job.ID, f.now.Add(-time.Hour), []byte("archive"), func(r *models.BackupRecord) {
		r.Manifest, r.HasManifest = manifest, true
	})
	f.admin.manifest = &models.Manifest{Collections: []models.CollectionManifest{
		{Name: "orders", DocumentsMin: 11, DocumentsMax: 11, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}, {Name: "sku_1", Keys: "sku:1", Unique: true}}},
		{Name: "users", DocumentsMin: 3, DocumentsMax: 3, Indexes: []models.IndexSpec{{Name: "_id_", Keys: "_id:1"}}},
	}}
	return f, job, rec
}

// neverTouchedSource fails when any MongoDB call named the source database.
func neverTouchedSource(t *testing.T, f *fixture, source string) {
	t.Helper()
	for _, c := range f.admin.touched() {
		if strings.HasSuffix(c, " "+source) {
			t.Fatalf("the source database was touched: %q", c)
		}
	}
	for _, r := range f.restorer.requests {
		if r.CloneDatabase == source || r.TargetDatabase != "" || !r.IsSafeClone() {
			t.Fatalf("restore request not into the temporary clone: %+v", r)
		}
	}
}

func TestRestoreTestSuccess(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerManual)
	if res.Status != models.RestoreTestOK || len(res.Mismatches) != 0 || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	wantTemp := "shop_rescue_verify_" + f.now.Format("20060102_150405")
	if res.TempDatabase != wantTemp || !res.Dropped || res.Collections != 2 || res.Documents != 14 || res.ConnectionID != "conn_src" {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(f.admin.dropped, []string{wantTemp}) {
		t.Fatalf("dropped = %v", f.admin.dropped)
	}
	if len(f.restorer.requests) != 1 || f.restorer.requests[0].CloneDatabase != wantTemp || *f.restorer.requests[0].Verify {
		t.Fatalf("restore request = %+v", f.restorer.requests)
	}
	neverTouchedSource(t, f, "shop")

	ctx := context.Background()
	if j, _ := f.st.GetJob(ctx, job.ID); j.LastRestoreTest == nil || j.LastRestoreTest.Status != models.RestoreTestOK {
		t.Fatalf("job summary = %+v", j.LastRestoreTest)
	}
	if b := f.get(t, rec.ID); b.LastRestoreTest == nil || b.LastRestoreTest.ID != res.ID {
		t.Fatalf("backup summary = %+v", b.LastRestoreTest)
	}
	list, err := f.svc.ListRestoreTests(ctx, job.ID, 0)
	if err != nil || len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("history = %+v, %v", list, err)
	}
	if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.RestoreTestSucceeded}) {
		t.Fatalf("events = %v", types)
	}
}

func TestRestoreTestCountMismatch(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	f.admin.manifest.Collections[0].DocumentsMin, f.admin.manifest.Collections[0].DocumentsMax = 4, 4
	f.admin.manifest.Collections[1].Indexes = nil
	res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerScheduled)
	if res.Status != models.RestoreTestMismatch || len(res.Mismatches) != 2 || !res.Dropped {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Mismatches[0], "orders: 4 documents restored, 10 to 12 expected") {
		t.Fatalf("mismatch detail = %q", res.Mismatches[0])
	}
	if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.RestoreTestFailed}) {
		t.Fatalf("events = %v", types)
	}
}

func TestRestoreTestAlwaysDropsTheTemporaryDatabase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		execute func(ctx context.Context, req models.RestoreRequest) error
		cancel  bool
		wantErr string
	}{
		{"restore fails", func(context.Context, models.RestoreRequest) error {
			return errors.New("mongorestore failed: exit status 1")
		}, false, "mongorestore failed"},
		{"panic", func(context.Context, models.RestoreRequest) error { panic("boom") }, false, "internal error"},
		{"cancelled", func(ctx context.Context, _ models.RestoreRequest) error {
			<-ctx.Done()
			return ctx.Err()
		}, true, "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, job, rec := restoreTestFixture(t)
			f.restorer.execute = tc.execute
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}
			res := f.svc.runRestoreTest(ctx, job, rec, TriggerScheduled)
			if res.Status != models.RestoreTestError || !strings.Contains(res.Error, tc.wantErr) {
				t.Fatalf("result = %+v", res)
			}
			if !res.Dropped || len(f.admin.dropped) != 1 || f.admin.dropped[0] != res.TempDatabase {
				t.Fatalf("the temporary database must be dropped: dropped=%v %v", res.Dropped, f.admin.dropped)
			}
			neverTouchedSource(t, f, "shop")
			// The failure is recorded and notified.
			if b := f.get(t, rec.ID); b.LastRestoreTest == nil || b.LastRestoreTest.Status != models.RestoreTestError {
				t.Fatalf("backup summary = %+v", b.LastRestoreTest)
			}
			if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.RestoreTestFailed}) {
				t.Fatalf("events = %v", types)
			}
		})
	}
}

func TestRestoreTestRefusals(t *testing.T) {
	t.Run("missing privileges", func(t *testing.T) {
		f, job, rec := restoreTestFixture(t)
		f.admin.missing = []string{"createCollection", "dropDatabase"}
		res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerScheduled)
		if res.Status != models.RestoreTestError || !strings.Contains(res.Error, "lacks the privileges") ||
			!strings.Contains(res.Error, "createCollection, dropDatabase") {
			t.Fatalf("result = %+v", res)
		}
		if len(f.restorer.requests) != 0 || len(f.admin.dropped) != 0 || res.TempDatabase != "" {
			t.Fatalf("nothing may run without privileges: restores=%d dropped=%v", len(f.restorer.requests), f.admin.dropped)
		}
		if types := f.pub.types(); !slices.Equal(types, []events.EventType{events.RestoreTestFailed}) {
			t.Fatalf("events = %v", types)
		}
	})
	t.Run("temporary database exists", func(t *testing.T) {
		f, job, rec := restoreTestFixture(t)
		f.admin.exists["shop_rescue_verify_"+f.now.Format("20060102_150405")] = true
		res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerScheduled)
		if res.Status != models.RestoreTestError || !strings.Contains(res.Error, "already exists") {
			t.Fatalf("result = %+v", res)
		}
		if len(f.restorer.requests) != 0 || len(f.admin.dropped) != 0 {
			t.Fatal("an existing database must be left untouched")
		}
	})
	t.Run("encrypted without key", func(t *testing.T) {
		f, job, rec := restoreTestFixture(t)
		rec.Encrypted = true
		f.restorer.canDecrypt = false
		res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerScheduled)
		if res.Status != models.RestoreTestError || !strings.Contains(res.Error, "no decryption key") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("no manifest", func(t *testing.T) {
		f, job, _ := restoreTestFixture(t)
		plain := f.putBackup(t, "bkp_plain", job.ID, f.now, []byte("x"), nil)
		res := f.svc.runRestoreTest(context.Background(), job, plain, TriggerScheduled)
		if res.Status != models.RestoreTestOK || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "no manifest") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("test connection", func(t *testing.T) {
		f, job, rec := restoreTestFixture(t)
		job.RestoreTest.ConnectionID = "conn_test"
		res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerScheduled)
		if res.Status != models.RestoreTestOK || res.ConnectionID != "conn_test" || f.restorer.requests[0].TargetConnectionID != "conn_test" {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestRestoreTestDue(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	ctx := context.Background()
	if !f.svc.restoreTestDue(ctx, job) {
		t.Fatal("a job never tested is due")
	}
	f.svc.AfterBackup(ctx, job, rec)
	if len(f.restorer.requests) != 1 {
		t.Fatalf("AfterBackup ran %d tests", len(f.restorer.requests))
	}
	current, _ := f.st.GetJob(ctx, job.ID)
	if f.svc.restoreTestDue(ctx, current) {
		t.Fatal("a weekly test that just ran is not due")
	}
	f.advance(7*24*time.Hour - 30*time.Minute) // within the slack
	if !f.svc.restoreTestDue(ctx, current) {
		t.Fatal("a weekly test is due again after a week")
	}
	disabled := current.Clone()
	disabled.RestoreTest.Enabled = false
	if f.svc.restoreTestDue(ctx, disabled) {
		t.Fatal("a disabled test is never due")
	}

	// Every 2 backups: the backups completed after the last test count.
	everyN := current.Clone()
	everyN.RestoreTest = &models.RestoreTestPolicy{Enabled: true, Frequency: models.RestoreTestEveryN, EveryN: 2}
	if f.svc.restoreTestDue(ctx, everyN) {
		t.Fatal("no backup since the last test")
	}
	f.putBackup(t, "bkp_2", job.ID, f.now, []byte("2"), nil)
	if f.svc.restoreTestDue(ctx, everyN) {
		t.Fatal("one backup since the last test")
	}
	f.putBackup(t, "bkp_3", job.ID, f.now.Add(time.Minute), []byte("3"), nil)
	if !f.svc.restoreTestDue(ctx, everyN) {
		t.Fatal("two backups since the last test")
	}
}

func TestStartRestoreTest(t *testing.T) {
	f, job, rec := restoreTestFixture(t)
	ctx := context.Background()
	got, err := f.svc.StartRestoreTest(ctx, job.ID)
	if err != nil || got.ID != rec.ID {
		t.Fatalf("start = %+v, %v", got, err)
	}
	f.waitIdle(t)
	if j, _ := f.st.GetJob(ctx, job.ID); j.LastRestoreTest == nil || j.LastRestoreTest.Status != models.RestoreTestOK {
		t.Fatalf("job summary = %+v", j.LastRestoreTest)
	}
	if _, err := f.svc.StartRestoreTest(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}
	empty := &models.Job{ID: "job_empty", Name: "e", Database: "other", CronExpression: "@daily"}
	if err := f.st.CreateJob(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartRestoreTest(ctx, empty.ID); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("job without backups = %v", err)
	}
	unavailable := New(Config{Store: f.st, Targets: f.targets, Runs: f.runs})
	if _, err := unavailable.StartRestoreTest(ctx, job.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("without the restore engine = %v", err)
	}
}
