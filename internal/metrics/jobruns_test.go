package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

func TestPerDatabaseAndPerRunSeries(t *testing.T) {
	m := New(BuildInfo{})
	ctx := context.Background()
	ts := time.Unix(1_790_000_000, 0).UTC()
	// A multi-database run: one event per database, then the run's summary.
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "all", Database: "shop", RunID: "run_1", InRun: true, SizeBytes: 100, Time: ts})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "all", Database: "billing", RunID: "run_1", InRun: true})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "all", RunID: "run_1", Duration: time.Minute,
		Run: &events.RunSummary{Status: "partial", Multi: true, Databases: 2, Succeeded: 1, Failed: 1}})
	// A single-database run carries its run summary on its one event.
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "one", Database: "crm", RunID: "run_2", SizeBytes: 5, Time: ts,
		Run: &events.RunSummary{Status: "ok", Databases: 1, Succeeded: 1}})

	out := scrape(t, m)
	for _, w := range []string{
		`mongorescue_backups_total{job="all",status="succeeded"} 1`,
		`mongorescue_backups_total{job="all",status="failed"} 1`,
		`mongorescue_database_backups_total{database="shop",job="all",status="succeeded"} 1`,
		`mongorescue_database_backups_total{database="billing",job="all",status="failed"} 1`,
		`mongorescue_database_backup_size_bytes{database="shop",job="all"} 100`,
		`mongorescue_database_last_successful_backup_timestamp_seconds{database="shop",job="all"} 1.79e+09`,
		`mongorescue_job_runs_total{job="all",status="partial"} 1`,
		`mongorescue_job_run_duration_seconds_count{job="all"} 1`,
		`mongorescue_job_runs_total{job="one",status="ok"} 1`,
		`mongorescue_backups_total{job="one",status="succeeded"} 1`,
		`mongorescue_database_backups_total{database="crm",job="one",status="succeeded"} 1`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	// The summary of a multi-database run is not a backup of its own.
	if strings.Contains(out, `mongorescue_backups_total{job="all",status="failed"} 2`) {
		t.Error("the run summary was counted as a backup")
	}

	m.ForgetJob("all")
	if out = scrape(t, m); strings.Contains(out, `job="all"`) {
		t.Error("ForgetJob must drop the per-database and per-run series too")
	}
}
