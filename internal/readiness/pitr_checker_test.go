package readiness_test

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestCheckerUsesThePITRRPO(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(48 * time.Hour)}
	pub := &recorder{}
	durable := 60.0
	stream := readiness.StreamInfo{ID: "pst_a", ConnectionID: "c1", Enabled: true, Running: true, DurableRPOSeconds: &durable, WindowOpen: true}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Publisher: pub, Now: clk.Now,
		Streams: func(context.Context) ([]readiness.StreamInfo, error) { return []readiness.StreamInfo{stream}, nil }})
	saveJob(t, st, &models.Job{ID: "job_a", Name: "shop", Database: "shop", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", RPOMinutes: 60, CreatedAt: t0})
	saveBackup(t, st, &models.BackupRecord{ID: "b1", JobID: "job_a", Database: "shop", ConnectionID: "c1", StartedAt: t0})

	check := func(want ...events.EventType) {
		t.Helper()
		if err := svc.Check(ctx); err != nil {
			t.Fatal(err)
		}
		evs := pub.take()
		if len(evs) != len(want) {
			t.Fatalf("events %v, want %v", evs, want)
		}
		for i, e := range evs {
			if e.Type != want[i] {
				t.Fatalf("events %v, want %v", evs, want)
			}
		}
	}

	// The job's backup is two days old, but the healthy stream covers the database.
	check()
	if b := breaches(t, st); len(b) != 0 {
		t.Fatalf("breaches %v while the stream covers the database", b)
	}
	// The stream falls behind the objective: the job RPO applies again.
	durable = 2 * 3600
	check(events.JobRPOMissed)
	// Caught up again.
	durable = 30
	check(events.JobRPORecovered)
	// A broken chain without a window no longer covers the database.
	stream.WindowOpen, stream.Broken = false, true
	check(events.JobRPOMissed)
	// Nor does a failing collector.
	stream.WindowOpen, stream.Broken = true, false
	check(events.JobRPORecovered)
	stream.Failing = true
	check(events.JobRPOMissed)
}
