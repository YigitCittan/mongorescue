package scheduler

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// An hourly job with a 01:00-05:00 window over three days: the real runs all stay
// in the history, the skips of each gap share one record, and backup.skipped is
// published once per gap, at most once a day.
func TestHourlyJobWithAWindowCoalescesSkips(t *testing.T) {
	metaStore := storetest.New(t)
	var dumps atomic.Int32
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		dumps.Add(1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))
	pub := &recordingPublisher{}
	start := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	now := start
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithPublisher(pub), WithClock(func() time.Time { return now }))
	job := &models.Job{ID: "job_hourly", Database: "shop", CronExpression: "CRON_TZ=UTC 0 * * * *", Enabled: true,
		BackupWindow: window(t, "01:00", "05:00", false)}
	ctx := context.Background()
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for h := range 72 {
		now = start.Add(time.Duration(h) * time.Hour)
		s.executeJob(ctx, job.ID)
	}
	if dumps.Load() != 12 {
		t.Fatalf("%d backups, want 4 a day", dumps.Load())
	}
	all, err := metaStore.ListJobRuns(ctx, job.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var skipped []*models.JobRun
	for _, r := range all {
		if r.Status == models.JobRunSkipped {
			skipped = append(skipped, r)
		}
	}
	// 05:00 to 00:00 each day: 20 activations per gap, three gaps.
	if len(all) != 15 || len(skipped) != 3 {
		t.Fatalf("%d runs with %d skip records, want 12 runs and 3 skip records", len(all)-len(skipped), len(skipped))
	}
	for _, r := range skipped {
		if r.SkippedRuns != 20 || r.CompletedAt == nil || r.CompletedAt.Sub(r.StartedAt) != 19*time.Hour {
			t.Fatalf("skip record %+v", r)
		}
	}
	// The history reads the newest executed runs, never the skips.
	executed, err := metaStore.ListExecutedJobRuns(ctx, job.ID, 12)
	if err != nil || len(executed) != 12 {
		t.Fatalf("executed runs %d, %v", len(executed), err)
	}
	for _, r := range executed {
		if r.Status == models.JobRunSkipped {
			t.Fatal("a skip in the executed runs")
		}
	}
	pub.mu.Lock()
	var notified []time.Time
	for _, e := range pub.got {
		if e.Type == events.BackupSkipped {
			notified = append(notified, e.Time)
		}
	}
	pub.mu.Unlock()
	if len(notified) != 3 {
		t.Fatalf("%d backup.skipped events over three days, want one per gap", len(notified))
	}
	perDay := map[string]int{}
	for _, at := range notified {
		perDay[at.Format(time.DateOnly)]++
	}
	for day, n := range perDay {
		if n > 1 {
			t.Fatalf("%d backup.skipped events on %s", n, day)
		}
	}
}
