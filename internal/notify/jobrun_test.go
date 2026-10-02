package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestOneNotificationPerJobRun checks that the per-database events of a
// multi-database run are not delivered: only the run's summary is.
func TestOneNotificationPerJobRun(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fake := &fakeNotifier{}
	svc := NewService(repo, WithNotifierFactory(func(*Channel) (Notifier, error) { return fake, nil }),
		WithRetryPolicy(RetryPolicy{Retries: 0, BaseBackoff: time.Millisecond, AttemptTimeout: time.Second}))
	if _, err := svc.CreateChannel(ctx, webhookChannel("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRule(ctx, &Rule{ID: "r", Name: "r", Enabled: true,
		Events: []events.EventType{events.BackupFailed, events.BackupSucceeded}, ChannelIDs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()

	run := &models.JobRun{ID: "run_1", JobID: "job_all", StartedAt: time.Now(), Databases: []models.JobRunDatabase{
		{Database: "shop", BackupID: "b1", Status: models.StatusCompleted},
		{Database: "billing", BackupID: "b2", Status: models.StatusFailed, Error: "exit status 1"},
		{Database: "gone", Status: models.StatusFailed, Error: models.ErrorDatabaseNotFound},
	}}
	run.Finish(time.Now())
	svc.HandleEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "job_all", Database: "shop", RunID: "run_1", InRun: true})
	svc.HandleEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "job_all", Database: "billing", RunID: "run_1", InRun: true})
	svc.HandleEvent(ctx, events.JobRunEvent(run))

	deadline := time.Now().Add(5 * time.Second)
	for fake.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := fake.count(); n != 1 {
		t.Fatalf("%d notifications sent; want one summary for the run", n)
	}
}

func TestRenderJobRunSummary(t *testing.T) {
	run := &models.JobRun{ID: "run_1", JobID: "nightly", StartedAt: time.Now(), NewDatabases: []string{"prod_new"}, Databases: []models.JobRunDatabase{
		{Database: "shop", Status: models.StatusCompleted},
		{Database: "billing", Status: models.StatusFailed, Error: "exit status 1"},
	}}
	run.Finish(time.Now())
	msg := Render(events.JobRunEvent(run))
	for _, want := range []string{
		"Backup partially failed: job nightly (1 of 2 databases backed up)",
		"billing: exit status 1",
		"Failed databases: billing",
		"1 new database(s) not included: prod_new",
		"Run ID: run_1",
	} {
		if !strings.Contains(msg.Body, want) {
			t.Errorf("body %q lacks %q", msg.Body, want)
		}
	}
	p := NewWebhookPayload(msg)
	if p.RunID != "run_1" || p.Run == nil || p.Run.Status != "partial" || p.Run.Failed != 1 {
		t.Errorf("webhook payload = %+v", p)
	}

	added := Render(events.DatabasesAddedEvent("nightly", "run_2", []string{"prod_c", "prod_d"}))
	if !strings.Contains(added.Subject, "job nightly now also backs up 2 database(s)") || !strings.Contains(added.Body, "prod_c, prod_d") {
		t.Errorf("databases added = %+v", added)
	}
	if !events.JobDatabasesAdded.Subscribable() {
		t.Error("job.databases_added must be selectable by rules")
	}
}
