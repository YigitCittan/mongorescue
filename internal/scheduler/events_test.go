package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// recordingPublisher captures published events.
type recordingPublisher struct {
	mu  sync.Mutex
	got []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, e events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, e)
	return true
}

func TestSchedulerPublishesBackupEvents(t *testing.T) {
	tests := []struct {
		name     string
		waitErr  error
		wantType events.EventType
	}{
		{"success", nil, events.BackupSucceeded},
		{"failure", errors.New("exit status 1"), events.BackupFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metaStore := storetest.New(t)
			mockStorage := storage.NewMockStorage()
			runner := func(context.Context, string, ...string) (io.ReadCloser, io.Reader, func() error, error) {
				return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader("stderr"), func() error { return tt.waitErr }, nil
			}
			engine := backup.NewEngine(mockStorage, "mongodb://localhost:27017", backup.WithRunner(runner))
			pub := &recordingPublisher{}
			sched := NewScheduler(metaStore, engine, mockStorage, nil, WithPublisher(pub))

			ctx := context.Background()
			job := &models.Job{ID: "job_shop", Name: "Shop", Database: "shop", CronExpression: "@daily", Enabled: true}
			if err := metaStore.SaveJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := sched.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer sched.Stop()
			if got := sched.ActiveJobCount(); got != 1 {
				t.Errorf("ActiveJobCount = %d; want 1", got)
			}

			_, _ = sched.TriggerJob(ctx, job.ID)

			pub.mu.Lock()
			defer pub.mu.Unlock()
			if len(pub.got) != 1 {
				t.Fatalf("published %d events; want 1", len(pub.got))
			}
			e := pub.got[0]
			if e.Type != tt.wantType || e.JobID != "job_shop" || e.Database != "shop" || e.BackupID == "" {
				t.Errorf("unexpected event %+v", e)
			}
		})
	}
}
