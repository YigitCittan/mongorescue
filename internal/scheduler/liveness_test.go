package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestLivenessTick(t *testing.T) {
	sched, _ := newTestScheduler(t)
	if !sched.LastTick().IsZero() || sched.Stale(time.Now()) {
		t.Fatal("a scheduler that never started has no tick and is not stale")
	}
	before := time.Now()
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	last := sched.LastTick()
	if last.Before(before) {
		t.Fatalf("Start did not record a tick: %v", last)
	}
	if sched.Stale(last.Add(StaleAfter)) {
		t.Fatal("a tick exactly StaleAfter old is not stale yet")
	}
	if !sched.Stale(last.Add(StaleAfter + time.Second)) {
		t.Fatal("a tick older than StaleAfter is stale")
	}

	// A hung scheduler: the tick cannot take the lock, so the last tick ages.
	sched.lastTick.Store(time.Now().Add(-time.Hour).UnixNano())
	if !sched.Stale(time.Now()) {
		t.Fatal("an hour-old tick is stale")
	}
	sched.tick()
	if sched.Stale(time.Now()) {
		t.Fatal("tick did not refresh the last tick")
	}

	sched.Stop()
	stopped := sched.LastTick()
	sched.tick()
	if !sched.LastTick().Equal(stopped) {
		t.Fatal("a stopped scheduler must not tick")
	}
}

// recordingObserver records the run notifications it receives.
type recordingObserver struct {
	mu     sync.Mutex
	events []string
	urls   []string
}

func (o *recordingObserver) JobRunStarted(job *models.Job, _ *models.JobRun) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "start")
	o.urls = append(o.urls, job.HeartbeatURL)
}

func (o *recordingObserver) JobRunFinished(job *models.Job, run *models.JobRun) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, string(run.Status))
	o.urls = append(o.urls, job.HeartbeatURL)
}

func TestRunObserverSeesStartAndOutcome(t *testing.T) {
	sched, metaStore := newTestScheduler(t)
	obs := &recordingObserver{}
	sched.observer = obs
	job := &models.Job{
		ID: "job_observed", Database: "analytics", CronExpression: "@daily", Enabled: true,
		HeartbeatURL: "https://hc.example.com/ping/abc",
	}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	stored, err := metaStore.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	if _, err = sched.runBackupForJob(context.Background(), stored); err != nil {
		t.Fatalf("run: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = sched.runBackupForJob(ctx, stored); err == nil {
		t.Fatal("a cancelled run must fail")
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 4 || obs.events[0] != "start" || obs.events[1] != string(models.JobRunOK) ||
		obs.events[2] != "start" || obs.events[3] == string(models.JobRunOK) {
		t.Fatalf("observer events = %v; want start, ok, start, a failure", obs.events)
	}
	for _, u := range obs.urls {
		if u != job.HeartbeatURL {
			t.Fatalf("observer saw heartbeat URL %q; want the decrypted URL", u)
		}
	}
}
