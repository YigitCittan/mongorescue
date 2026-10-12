package scheduler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// renewingFence is an HA fence that holds the lease until a renewal finds that it
// expired during a suspend.
type renewingFence struct {
	renewals atomic.Int32
	expired  atomic.Bool
}

func (f *renewingFence) Check() error {
	if f.expired.Load() {
		return errors.New("this instance does not hold the HA lease")
	}
	return nil
}
func (f *renewingFence) Epoch() uint64 { return 1 }
func (f *renewingFence) Renew(context.Context) error {
	f.renewals.Add(1)
	f.expired.Store(true) // the lease expired while the machine was suspended
	return f.Check()
}

// TestNoFireAfterASuspendWithoutTheLease simulates a suspended machine: the wall
// clock moves two minutes while the monotonic clock does not. The next cron fire
// renews the HA lease first, finds it lost, and runs nothing.
func TestNoFireAfterASuspendWithoutTheLease(t *testing.T) {
	metaStore := storetest.New(t)
	var dumps atomic.Int32
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		dumps.Add(1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	clock := &steppedClock{}
	fence := &renewingFence{}
	s := NewScheduler(metaStore, backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner)),
		storage.NewMockStorage(), nil, WithClock(clock.now), WithFence(fence))
	job := &models.Job{ID: "job_minutely", Database: "shop", CronExpression: "* * * * *", Enabled: true}
	if err := metaStore.SaveJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	s.cronFire(job.ID) // no jump: fires
	if dumps.Load() != 1 || fence.renewals.Load() != 0 {
		t.Fatalf("dumps %d, renewals %d before the suspend; want 1 and 0", dumps.Load(), fence.renewals.Load())
	}
	clock.step(2 * time.Minute) // the suspend
	s.mu.Lock()
	s.lastTrigger[job.ID] = time.Now().Add(-time.Hour) // not a repeated trigger
	s.mu.Unlock()
	s.cronFire(job.ID)
	if fence.renewals.Load() != 1 {
		t.Fatalf("%d renewals after the wall clock jump; want 1", fence.renewals.Load())
	}
	if dumps.Load() != 1 {
		t.Fatal("a scheduled backup ran after the lease expired during a suspend")
	}
}
