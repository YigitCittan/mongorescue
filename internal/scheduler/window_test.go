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
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func window(t *testing.T, start, end string, cancelAtEnd bool) *models.BackupWindow {
	t.Helper()
	w := &models.BackupWindow{Start: start, End: end, CancelAtWindowEnd: cancelAtEnd}
	if err := w.Normalize(); err != nil {
		t.Fatal(err)
	}
	return w
}

func fixedClock(s string) func() time.Time {
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return at }
}

func TestScheduledRunOutsideTheWindowIsSkipped(t *testing.T) {
	metaStore := storetest.New(t)
	var dumps atomic.Int32
	runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
		dumps.Add(1)
		return io.NopCloser(strings.NewReader("archive")), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(runner))
	pub := &recordingPublisher{}
	s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithPublisher(pub),
		WithClock(fixedClock("2026-10-05T12:00:00Z")))
	job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "0 * * * *", Enabled: true,
		BackupWindow: window(t, "01:00", "05:00", false)}
	ctx := context.Background()
	if err := metaStore.SaveJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterJob(job); err != nil {
		t.Fatal(err)
	}

	s.executeJob(ctx, job.ID)
	if dumps.Load() != 0 {
		t.Fatal("a scheduled run outside the window started")
	}
	list, err := metaStore.ListJobRuns(ctx, job.ID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("runs %v, %v", list, err)
	}
	if r := list[0]; r.Status != models.JobRunSkipped || r.SkipReason != models.SkipOutsideWindow || r.CompletedAt == nil || r.Trigger != models.TriggerScheduled {
		t.Fatalf("recorded run %+v", r)
	}
	pub.mu.Lock()
	got := append([]events.Event(nil), pub.got...)
	pub.mu.Unlock()
	if len(got) != 1 || got[0].Type != events.BackupSkipped || got[0].Detail != "skipped (outside window)" || got[0].Database != "shop" {
		t.Fatalf("events %+v", got)
	}
	stored, err := metaStore.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastRun != nil || stored.NextRun == nil {
		t.Fatalf("a skipped run must refresh the next run only: last %v, next %v", stored.LastRun, stored.NextRun)
	}
	// A skipped run is not the job's latest run.
	latest, err := metaStore.LatestJobRuns(ctx, []string{job.ID})
	if err != nil || latest[job.ID] != nil {
		t.Fatalf("latest runs %v, %v", latest, err)
	}

	// Manual runs ignore the window.
	if _, err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if dumps.Load() != 1 {
		t.Fatal("a manual run must ignore the backup window")
	}

	// Inside the window the scheduled run starts.
	s.now = fixedClock("2026-10-05T02:00:00Z")
	s.executeJob(ctx, job.ID)
	if dumps.Load() != 2 {
		t.Fatal("a scheduled run inside the window did not start")
	}
}

// blockingRunner is a mongodump that runs until its context ends.
func blockingRunner(started chan<- struct{}) backup.ProcessRunner {
	return func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		started <- struct{}{}
		return pr, strings.NewReader(""), func() error { <-ctx.Done(); return ctx.Err() }, nil
	}
}

func TestCancelAtWindowEnd(t *testing.T) {
	for _, cancelAtEnd := range []bool{true, false} {
		name := "cancels"
		if !cancelAtEnd {
			name = "keeps running"
		}
		t.Run(name, func(t *testing.T) {
			metaStore := storetest.New(t)
			started := make(chan struct{}, 1)
			engine := backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", backup.WithRunner(blockingRunner(started)))
			reg := runs.NewRegistry()
			// 300ms before the window closes.
			s := NewScheduler(metaStore, engine, storage.NewMockStorage(), nil, WithRunRegistry(reg),
				WithClock(fixedClock("2026-10-05T10:59:59.7Z")))
			job := &models.Job{ID: "job_shop", Database: "shop", CronExpression: "@hourly", BackupWindow: window(t, "10:00", "11:00", cancelAtEnd)}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if err := metaStore.SaveJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.executeJob(ctx, job.ID)
			}()
			<-started
			select {
			case <-done:
				if !cancelAtEnd {
					t.Fatal("the run ended without cancel_at_window_end")
				}
			case <-time.After(3 * time.Second):
				if cancelAtEnd {
					t.Fatal("the run was not cancelled at the window's end")
				}
				stop()
				<-done
				return
			}
			list, err := metaStore.ListJobRuns(context.Background(), job.ID, 1)
			if err != nil || len(list) != 1 || list[0].Status != models.JobRunCancelled {
				t.Fatalf("run %+v, %v", list, err)
			}
			rec, err := metaStore.GetBackupRecord(context.Background(), list[0].Databases[0].BackupID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status != models.StatusCancelled || !strings.Contains(rec.ErrorMessage, WindowEndReason) {
				t.Fatalf("backup %s: %q", rec.Status, rec.ErrorMessage)
			}
		})
	}
}

func TestEffectiveRPOWithAWindow(t *testing.T) {
	from := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	job := &models.Job{CronExpression: "CRON_TZ=UTC 0 * * * *"}
	plain, _ := EffectiveRPO(job, from)
	if plain != models.DefaultRPOFloor {
		t.Fatalf("hourly without a window: %v, want the floor", plain)
	}
	w := window(t, "01:00", "05:00", false)
	w.Timezone = "UTC"
	job.BackupWindow = w
	// Allowed runs at 01, 02, 03 and 04 o'clock: the largest gap is 21 hours.
	if got := WindowedInterval(job.CronExpression, w, from); got != 21*time.Hour {
		t.Fatalf("interval %v, want 21h", got)
	}
	rpo, isDefault := EffectiveRPO(job, from)
	if !isDefault || rpo != 43*time.Hour {
		t.Fatalf("RPO %v (default %v), want 43h", rpo, isDefault)
	}
	// Weekdays only: Friday 04:00 to Monday 01:00.
	w.Days = []string{"mon", "tue", "wed", "thu", "fri"}
	if got := WindowedInterval(job.CronExpression, w, from); got != 69*time.Hour {
		t.Fatalf("weekday interval %v, want 69h", got)
	}
	// A window no activation falls into leaves no interval (the daily fallback).
	if got := WindowedInterval("CRON_TZ=UTC 0 12 * * *", window(t, "01:00", "05:00", false), from); got != 0 {
		t.Fatalf("interval %v, want 0", got)
	}
	job.RPOMinutes = 120
	if rpo, isDefault = EffectiveRPO(job, from); isDefault || rpo != 2*time.Hour {
		t.Fatalf("explicit RPO %v", rpo)
	}
}
