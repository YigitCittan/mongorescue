package app

import (
	"context"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// A job write that reaches only the scheduler (an applied pending change, an
// approval) drops the cached overview history, as API writes do.
func TestSchedulerJobWritesDropTheCachedHistory(t *testing.T) {
	application, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ctx := context.Background()
	first, err := application.ops.History(ctx, operations.HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := application.ops.History(ctx, operations.HistoryRequest{}); again != first {
		t.Fatal("the history is not cached")
	}
	job := &models.Job{ID: "job_sched", Name: "nightly", Database: "shop", CronExpression: "0 3 * * *", Enabled: true}
	if err = application.scheduler.ApplyJobUpdate(job, func() error { return application.metaStore.CreateJob(ctx, job) }); err != nil {
		t.Fatal(err)
	}
	h, err := application.ops.History(ctx, operations.HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if h == first {
		t.Fatal("a scheduler job write kept the cached history")
	}
	if _, ok := h.Jobs["job_sched"]; !ok {
		t.Fatalf("the new job is missing from the history: %v", h.Jobs)
	}
}
