package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestLivenessTick(t *testing.T) {
	sched, _ := newTestScheduler(t)
	if !sched.LastTick().IsZero() || sched.Stale() {
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
	if sched.Stale() {
		t.Fatal("a fresh tick is not stale")
	}
	// The tick keeps its monotonic clock reading ("m=±..." in its String form), so
	// Stale measures its age with the monotonic clock and a wall clock step (NTP, a
	// resumed VM) cannot make it stale.
	if !strings.Contains(last.String(), " m=") {
		t.Fatalf("the tick %s has no monotonic clock reading", last)
	}
	if wall := last.Round(0); strings.Contains(wall.String(), " m=") {
		t.Fatal("Round(0) should strip the monotonic reading (test assumption)")
	}

	// A hung scheduler: the tick cannot take the lock, so the last tick ages.
	old := time.Now().Add(-StaleAfter - time.Second)
	sched.lastTick.Store(&old)
	if !sched.Stale() {
		t.Fatal("a tick older than StaleAfter is stale")
	}
	sched.tick()
	if sched.Stale() || !strings.Contains(sched.LastTick().String(), " m=") {
		t.Fatal("tick did not refresh the last tick with a monotonic reading")
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

func (o *recordingObserver) JobRunFinished(job *models.Job, run *models.JobRun, interrupted bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	event := string(run.Status)
	if interrupted {
		event += " interrupted"
	}
	o.events = append(o.events, event)
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
	// The second run's context was cancelled, as a shutdown does: the observer is
	// told the run was interrupted, so its heartbeat sends no /fail.
	if len(obs.events) != 4 || obs.events[0] != "start" || obs.events[1] != string(models.JobRunOK) ||
		obs.events[2] != "start" || !strings.HasSuffix(obs.events[3], " interrupted") {
		t.Fatalf("observer events = %v; want start, ok, start, an interrupted run", obs.events)
	}
	for _, u := range obs.urls {
		if u != job.HeartbeatURL {
			t.Fatalf("observer saw heartbeat URL %q; want the decrypted URL", u)
		}
	}
}
