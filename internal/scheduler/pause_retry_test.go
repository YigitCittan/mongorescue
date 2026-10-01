package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPausedJobResumesAfterALateLoad covers a job paused until a time that has
// passed when the first listing of the jobs failed: once a retry loaded the jobs,
// the minute check resumes it and registers its schedule.
func TestPausedJobResumesAfterALateLoad(t *testing.T) {
	fileStore := storetest.New(t)
	past := time.Now().Add(-time.Minute)
	for _, j := range []*models.Job{
		{ID: "job_on", Name: "on", Database: "a", CronExpression: "@daily", Enabled: true},
		{ID: "job_due", Name: "due", Database: "b", CronExpression: "@daily", PausedUntil: &past},
	} {
		if err := fileStore.SaveJob(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	flaky := &flakyListStore{Store: fileStore, failures: 2}
	sched := NewScheduler(flaky, backup.NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017"), storage.NewMockStorage(), nil)
	sched.retryFirst, sched.retryLimit = time.Millisecond, 2*time.Millisecond
	defer sched.Stop()
	if err := sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitActive(t, sched, 1) // loaded by a retry: the paused job is not registered

	sched.resumeDueJobs()
	waitActive(t, sched, 2)
	job, err := fileStore.GetJob(context.Background(), "job_due")
	if err != nil || !job.Enabled || job.PausedUntil != nil || job.NextRun == nil {
		t.Fatalf("job_due = %+v, %v; want it resumed and scheduled", job, err)
	}
}
