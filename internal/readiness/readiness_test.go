package readiness_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// recorder is an events.Publisher keeping what it was given.
type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) Publish(_ context.Context, e events.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return true
}

// take returns and clears the recorded events.
func (r *recorder) take() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// newService returns a readiness service on st with the clock and publisher.
func newService(st *store.SQLiteStore, clk *clock, pub events.Publisher, observe func([]readiness.Sample)) *readiness.Service {
	return readiness.New(readiness.Config{Store: st, Publisher: pub, Now: clk.Now, Observe: observe})
}

func saveJob(t *testing.T, st *store.SQLiteStore, job *models.Job) {
	t.Helper()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = t0
	}
	if err := st.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func saveBackup(t *testing.T, st *store.SQLiteStore, b *models.BackupRecord) {
	t.Helper()
	if b.Status == "" {
		b.Status = models.StatusCompleted
	}
	if b.CompletedAt == nil {
		done := b.StartedAt.Add(time.Minute)
		b.CompletedAt = &done
	}
	if err := st.SaveBackupRecord(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func breaches(t *testing.T, st *store.SQLiteStore) []models.RPOBreach {
	t.Helper()
	list, err := st.ListRPOBreaches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestCheckerBreachRecoverAndNoRealertAfterRestart(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(2 * time.Hour)}
	pub := &recorder{}
	var samples []readiness.Sample
	svc := newService(st, clk, pub, func(s []readiness.Sample) { samples = s })

	// Hourly: the default objective is the six-hour floor.
	saveJob(t, st, &models.Job{ID: "job_h", Name: "hourly shop", Database: "shop", CronExpression: "@hourly", Enabled: true, ConnectionID: "c1"})
	saveBackup(t, st, &models.BackupRecord{ID: "b1", JobID: "job_h", Database: "shop", StartedAt: t0.Add(time.Hour)})

	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Fatalf("events within the objective = %+v", got)
	}
	if len(samples) != 1 || samples[0].JobID != "job_h" || samples[0].Target != 6*time.Hour ||
		!samples[0].Since.Equal(t0.Add(time.Hour+time.Minute)) {
		t.Fatalf("samples = %+v", samples)
	}

	// Seven hours after the backup the objective is missed: one event, one row.
	clk.set(t0.Add(8 * time.Hour))
	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got := pub.take()
	if len(got) != 1 || got[0].Type != events.JobRPOMissed || got[0].JobID != "job_h" || got[0].Database != "shop" ||
		got[0].BackupID != "b1" || got[0].Detail == "" {
		t.Fatalf("events after the breach = %+v", got)
	}
	if b := breaches(t, st); len(b) != 1 || !b[0].Since.Equal(t0.Add(8*time.Hour)) {
		t.Fatalf("breaches = %+v", b)
	}

	// The same breach is never reported again: not by the next check, not after a
	// restart (a new service on the same database).
	clk.set(t0.Add(9 * time.Hour))
	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newService(st, clk, pub, nil)
	if err := restarted.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if again := pub.take(); len(again) != 0 {
		t.Fatalf("re-alerted: %+v", again)
	}

	// A fresh backup heals it: one recovered event, the row is gone.
	saveBackup(t, st, &models.BackupRecord{ID: "b2", JobID: "job_h", Database: "shop", StartedAt: t0.Add(9 * time.Hour)})
	clk.set(t0.Add(9*time.Hour + 5*time.Minute))
	if err := restarted.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got = pub.take()
	if len(got) != 1 || got[0].Type != events.JobRPORecovered || got[0].Database != "shop" || got[0].Status != "recovered" {
		t.Fatalf("events after the recovery = %+v", got)
	}
	if b := breaches(t, st); len(b) != 0 {
		t.Fatalf("breaches after the recovery = %+v", b)
	}

	// A new breach after the recovery is reported again.
	clk.set(t0.Add(20 * time.Hour))
	if err := restarted.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got = pub.take(); len(got) != 1 || got[0].Type != events.JobRPOMissed {
		t.Fatalf("events after the second breach = %+v", got)
	}
}

func TestCheckerExplicitRPOAndPausedJobs(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(3 * time.Hour)}
	pub := &recorder{}
	svc := newService(st, clk, pub, nil)

	// A 90-minute objective on a daily job, never backed up: it counts from the job's
	// creation.
	job := &models.Job{ID: "job_d", Name: "daily", Database: "crm", CronExpression: "@daily", Enabled: true, RPOMinutes: 90}
	saveJob(t, st, job)
	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got := pub.take()
	if len(got) != 1 || got[0].Type != events.JobRPOMissed || got[0].BackupID != "" {
		t.Fatalf("events = %+v", got)
	}

	// Pausing the job drops the breach silently.
	job.Enabled = false
	saveJob(t, st, job)
	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Fatalf("events after pausing = %+v", got)
	}
	if b := breaches(t, st); len(b) != 0 {
		t.Fatalf("breaches of a paused job = %+v", b)
	}
}

func TestCheckerMultiDatabaseJob(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(30 * time.Hour)}
	pub := &recorder{}
	svc := newService(st, clk, pub, nil)

	saveJob(t, st, &models.Job{
		ID: "job_m", Name: "all", CronExpression: "@every 6h", Enabled: true, ConnectionID: "c1",
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"app_a", "app_b"}},
	})
	// app_a is fresh, app_b's newest backup is a day old (objective 13 hours).
	saveBackup(t, st, &models.BackupRecord{ID: "ba", JobID: "job_m", Database: "app_a", StartedAt: t0.Add(29 * time.Hour)})
	saveBackup(t, st, &models.BackupRecord{ID: "bb", JobID: "job_m", Database: "app_b", StartedAt: t0.Add(6 * time.Hour)})
	if err := svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got := pub.take()
	if len(got) != 1 || got[0].Database != "app_b" || got[0].Type != events.JobRPOMissed {
		t.Fatalf("events = %+v", got)
	}
}

func TestHandleEventKicksAnEarlyCheck(t *testing.T) {
	st := storetest.New(t)
	clk := &clock{now: t0.Add(10 * time.Hour)}
	checked := make(chan struct{}, 4)
	svc := readiness.New(readiness.Config{
		Store: st, Now: clk.Now, StartDelay: time.Hour, CheckInterval: time.Hour,
		Observe: func([]readiness.Sample) { checked <- struct{}{} },
	})
	svc.Start(context.Background())
	defer svc.Stop()
	// Manual backups and other events do not ask for a check.
	svc.HandleEvent(context.Background(), events.Event{Type: events.BackupSucceeded})
	svc.HandleEvent(context.Background(), events.Event{Type: events.RestoreSucceeded, JobID: "x"})
	select {
	case <-checked:
		t.Fatal("checked without a job backup")
	case <-time.After(50 * time.Millisecond):
	}
	svc.HandleEvent(context.Background(), events.Event{Type: events.BackupSucceeded, JobID: "job_h"})
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("a job's backup did not trigger a check")
	}
}

func TestReportRows(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(48 * time.Hour)}
	escrowed := false
	svc := readiness.New(readiness.Config{Store: st, Now: clk.Now, KeysEscrowed: func() bool { return escrowed }})

	// A multi-database job (every 6 hours, objective 13 hours) on c1: app_a has a
	// fresh, verified, restore-tested backup; app_b only an old one; app_c none.
	saveJob(t, st, &models.Job{
		ID: "job_m", Name: "apps", CronExpression: "@every 6h", Enabled: true, ConnectionID: "c1", CreatedAt: t0,
		DatabaseSelection: models.DatabaseSelection{Mode: models.SelectionList, Databases: []string{"app_a", "app_b", "app_c"}},
	})
	saveBackup(t, st, &models.BackupRecord{ID: "ba", JobID: "job_m", Database: "app_a", StartedAt: t0.Add(47 * time.Hour),
		Verification: models.VerificationOK, Encrypted: true})
	saveBackup(t, st, &models.BackupRecord{ID: "bb", JobID: "job_m", Database: "app_b", StartedAt: t0.Add(10 * time.Hour)})
	done := t0.Add(47*time.Hour + 30*time.Minute)
	if err := st.SaveRestoreTest(ctx, &models.RestoreTestResult{ID: "rt1", JobID: "job_m", Database: "app_a", Status: models.RestoreTestOK,
		StartedAt: t0.Add(47*time.Hour + 20*time.Minute), CompletedAt: &done, DurationSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	// A second job, created an hour ago, on the same app_a: no backups yet.
	saveJob(t, st, &models.Job{ID: "job_n", Name: "new", Database: "app_a", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", CreatedAt: t0.Add(47 * time.Hour)})
	// A paused job on c2 with a backup, never restore-tested but restored for real.
	saveJob(t, st, &models.Job{ID: "job_p", Name: "paused", Database: "app_a", CronExpression: "@daily", ConnectionID: "c2"})
	saveBackup(t, st, &models.BackupRecord{ID: "bp", JobID: "job_p", Database: "app_a", ConnectionID: "c2", StartedAt: t0.Add(40 * time.Hour)})
	restored := t0.Add(41 * time.Hour)
	if err := st.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rs1", SourceDatabase: "app_a", SourceConnectionID: "c2",
		Status: models.RestoreStatusCompleted, StartedAt: t0.Add(40*time.Hour + 30*time.Minute), CompletedAt: &restored, DurationSeconds: 1800}); err != nil {
		t.Fatal(err)
	}

	report, err := svc.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 4 {
		t.Fatalf("rows = %+v", report.Rows)
	}
	rows := map[string]readiness.Row{}
	for _, r := range report.Rows {
		rows[r.ConnectionID+"/"+r.Database] = r
	}

	a := rows["c1/app_a"]
	if len(a.Jobs) != 2 || a.LastGoodBackup == nil || a.LastGoodBackup.ID != "ba" || a.LastVerifiedBackup == nil ||
		a.LastRestoreTest == nil || a.LastRestoreTest.ID != "rt1" || a.RTO == nil || a.RTO.Source != readiness.RTOSourceRestoreTest ||
		a.RTO.Seconds != 600 || a.RPO.Met == nil || !*a.RPO.Met || a.RPO.TargetSeconds != (13*time.Hour).Seconds() {
		t.Fatalf("c1/app_a = %+v", a)
	}
	// Encrypted without an escrowed key: a warning, nothing else.
	if a.Status != readiness.StatusWarn || !slices.Equal(a.Reasons, []string{readiness.ReasonKeysNotEscrowed}) {
		t.Fatalf("c1/app_a status = %s %v", a.Status, a.Reasons)
	}
	if j := a.Jobs[1]; j.ID != "job_n" || !j.NoBackup || !j.Met || !j.Default {
		t.Fatalf("c1/app_a job_n = %+v", j)
	}

	b := rows["c1/app_b"]
	if b.Status != readiness.StatusFail || b.Reasons[0] != readiness.ReasonRPOMissed || b.RPO.Met == nil || *b.RPO.Met ||
		!slices.Contains(b.Reasons, readiness.ReasonNoRestoreTest) || !slices.Contains(b.Reasons, readiness.ReasonNotVerified) || b.RTO != nil {
		t.Fatalf("c1/app_b = %+v", b)
	}

	c := rows["c1/app_c"]
	if c.LastGoodBackup != nil || c.RPO.AgeSeconds != nil || c.Status != readiness.StatusFail || c.Reasons[0] != readiness.ReasonRPOMissed {
		t.Fatalf("c1/app_c = %+v", c)
	}

	p := rows["c2/app_a"]
	if p.RPO.Met != nil || p.RTO == nil || p.RTO.Source != readiness.RTOSourceRestore || p.RTO.Seconds != 1800 ||
		p.Status != readiness.StatusWarn || !slices.Contains(p.Reasons, readiness.ReasonPaused) || !slices.Contains(p.Reasons, readiness.ReasonNoRestoreTest) {
		t.Fatalf("c2/app_a = %+v", p)
	}

	// Fail rows come first; the summary counts every row.
	if report.Rows[0].Status != readiness.StatusFail || report.Summary != (readiness.Summary{Warn: 2, Fail: 2}) {
		t.Fatalf("order/summary = %s %+v", report.Rows[0].Status, report.Summary)
	}

	// With the keys escrowed and a failed restore test after the good one, app_a fails.
	escrowed = true
	failed := t0.Add(47*time.Hour + 50*time.Minute)
	if err = st.SaveRestoreTest(ctx, &models.RestoreTestResult{ID: "rt2", JobID: "job_m", Database: "app_a", Status: models.RestoreTestMismatch,
		StartedAt: t0.Add(47*time.Hour + 40*time.Minute), CompletedAt: &failed, DurationSeconds: 700}); err != nil {
		t.Fatal(err)
	}
	report, err = svc.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report.Rows {
		if r.ConnectionID == "c1" && r.Database == "app_a" {
			if r.Status != readiness.StatusFail || !slices.Equal(r.Reasons, []string{readiness.ReasonRestoreTestFailed}) ||
				r.LastRestoreTest.ID != "rt2" || r.RTO == nil || r.RTO.ID != "rt1" || !r.KeysEscrowed {
				t.Fatalf("c1/app_a after a failed test = %+v", r)
			}
		}
	}
}

func TestReportWithoutJobs(t *testing.T) {
	svc := readiness.New(readiness.Config{Store: storetest.New(t)})
	report, err := svc.Report(context.Background())
	if err != nil || report.Rows == nil || len(report.Rows) != 0 {
		t.Fatalf("Report = %+v, %v; want no rows", report, err)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0m", 45 * time.Second: "0m", 45 * time.Minute: "45m", 7*time.Hour + 12*time.Minute: "7h 12m",
		6 * time.Hour: "6h", 51*time.Hour + 5*time.Minute: "2d 3h", 14 * 24 * time.Hour: "14d",
	} {
		if got := readiness.FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q; want %q", d, got, want)
		}
	}
}
