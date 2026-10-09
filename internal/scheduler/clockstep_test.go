package scheduler

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// steppedClock is a wall clock that can be stepped like NTP or a VM restore does.
type steppedClock struct{ offset atomic.Int64 }

func (c *steppedClock) now() time.Time {
	return time.Now().Round(0).Add(time.Duration(c.offset.Load()))
}

func (c *steppedClock) step(d time.Duration) { c.offset.Add(int64(d)) }

// waitTicks waits until the scheduler records n more liveness ticks.
func waitTicks(t *testing.T, s *Scheduler, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < n; i++ {
		last := s.LastTick()
		for !s.LastTick().After(last) {
			if time.Now().After(deadline) {
				t.Fatalf("the liveness tick stopped after %d of %d ticks", i, n)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// TestLivenessTickSurvivesClockSteps covers #137: the tick runs on a monotonic
// ticker, so stepping the wall clock back an hour (or forward a day) neither stops
// it nor makes the scheduler stale.
func TestLivenessTickSurvivesClockSteps(t *testing.T) {
	metaStore := storetest.New(t)
	clock := &steppedClock{}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017")
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil,
		WithClock(clock.now), WithTickInterval(5*time.Millisecond))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitTicks(t, s, 2)

	clock.step(-time.Hour)
	waitTicks(t, s, 3)
	if s.Stale() {
		t.Fatal("a backward clock step made the scheduler stale")
	}
	clock.step(25 * time.Hour)
	waitTicks(t, s, 3)
	if s.Stale() {
		t.Fatal("a forward clock step made the scheduler stale")
	}

	// Stop ends the ticker goroutine: no tick after it.
	s.Stop()
	last := s.LastTick()
	time.Sleep(20 * time.Millisecond)
	if !s.LastTick().Equal(last) {
		t.Fatal("the scheduler ticked after Stop")
	}
}

// newTriggerFixture returns a scheduler with the daily job "job_daily" registered
// and a counter of its dumps.
func newTriggerFixture(t *testing.T, metaStore store.Store) (*Scheduler, *atomic.Int32) {
	t.Helper()
	var dumps atomic.Int32
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		dumps.Add(1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil)
	job := &models.Job{ID: "job_daily", Database: "shop", CronExpression: "0 2 * * *", Enabled: true}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterJob(job); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s, &dumps
}

// TestRepeatedTriggerIsSkipped checks the guard against a run repeated after a
// backward clock step: a second trigger of a daily job within half a day is
// skipped, a trigger a day later runs.
func TestRepeatedTriggerIsSkipped(t *testing.T) {
	s, dumps := newTriggerFixture(t, storetest.New(t))

	s.cronFire("job_daily")
	if dumps.Load() != 1 {
		t.Fatalf("dumps = %d after the first trigger; want 1", dumps.Load())
	}
	s.cronFire("job_daily") // the same slot again, as after a backward step
	if dumps.Load() != 1 {
		t.Fatalf("dumps = %d; a repeated trigger must be skipped", dumps.Load())
	}

	// The next day's trigger runs.
	s.mu.Lock()
	s.lastTrigger["job_daily"] = time.Now().Add(-24 * time.Hour)
	s.mu.Unlock()
	s.cronFire("job_daily")
	if dumps.Load() != 2 {
		t.Fatalf("dumps = %d after a trigger a day later; want 2", dumps.Load())
	}
}

// TestRepeatedTriggerAfterRestart checks the first trigger after a start against the
// newest scheduled run on record: one that started (by the wall clock) less than
// half a day ago, here an hour in the future after the clock was set back, makes
// the trigger a repeat; an older one does not.
func TestRepeatedTriggerAfterRestart(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		startedAgo time.Duration
		want       int32
	}{
		"clock set back": {startedAgo: -time.Hour, want: 0},
		"previous day":   {startedAgo: 24 * time.Hour, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			metaStore := storetest.New(t)
			s, dumps := newTriggerFixture(t, metaStore)
			prev := newRunOf("job_daily", models.TriggerScheduled)
			prev.StartedAt = time.Now().UTC().Add(-tc.startedAgo)
			prev.Status = models.JobRunOK
			if err := metaStore.SaveJobRun(ctx, prev); err != nil {
				t.Fatal(err)
			}
			// An on-demand run is not a scheduled slot and never counts.
			manual := newRunOf("job_daily", models.TriggerOnDemand)
			manual.Status = models.JobRunOK
			if err := metaStore.SaveJobRun(ctx, manual); err != nil {
				t.Fatal(err)
			}
			s.cronFire("job_daily")
			if dumps.Load() != tc.want {
				t.Fatalf("dumps = %d; want %d", dumps.Load(), tc.want)
			}
		})
	}
}
