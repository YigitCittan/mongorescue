//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// rpoEvents collects the RPO events published by the checker.
type rpoEvents struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *rpoEvents) Publish(_ context.Context, e events.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, e)
	return true
}

func (r *rpoEvents) take() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.list
	r.list = nil
	return out
}

// TestReadinessAfterRealBackup runs a job's backup of a real database and checks
// the readiness report and the RPO checker against it: the fresh backup meets the
// default objective, seven hours later the objective is missed exactly once (also
// across a restart of the checker), and the breach heals when the backup is recent
// again.
func TestReadinessAfterRealBackup(t *testing.T) {
	env := requireMongo(t)
	db := env.uniqueDB(t, "rpo")
	env.seed(t, db, "orders", 20)
	ctx := context.Background()
	metaStore := storetest.New(t)
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.NewScheduler(metaStore, newBackupEngine(env, st), st, discardLogger)
	job := &models.Job{ID: "job_rpo", Name: "hourly", Database: db, CronExpression: "@hourly", ConnectionID: "conn", Gzip: true, Enabled: true}
	if err = metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err = sched.TriggerJob(ctx, job.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	setClock := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	pub := &rpoEvents{}
	newChecker := func() *readiness.Service {
		return readiness.New(readiness.Config{Store: metaStore, Publisher: pub, Now: clock, Logger: discardLogger})
	}
	svc := newChecker()

	report, err := svc.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("rows = %+v", report.Rows)
	}
	row := report.Rows[0]
	if row.Database != db || row.ConnectionID != "conn" || row.LastGoodBackup == nil || row.RPO.Met == nil || !*row.RPO.Met ||
		row.RPO.TargetSeconds != (6*time.Hour).Seconds() || row.Status == readiness.StatusFail {
		t.Fatalf("row after the backup = %+v", row)
	}

	if err = svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Fatalf("events right after the backup = %+v", got)
	}

	setClock(now.Add(7 * time.Hour))
	if err = svc.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 1 || got[0].Type != events.JobRPOMissed || got[0].Database != db {
		t.Fatalf("events seven hours later = %+v", got)
	}
	if err = newChecker().Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Fatalf("a restarted checker alerted again: %+v", got)
	}

	setClock(time.Now().UTC().Add(time.Minute))
	if err = newChecker().Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 1 || got[0].Type != events.JobRPORecovered {
		t.Fatalf("events after the recovery = %+v", got)
	}
}
