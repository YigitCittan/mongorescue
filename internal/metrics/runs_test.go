package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

func TestCancelledRunsAndActiveRuns(t *testing.T) {
	m := New(BuildInfo{})
	out := scrape(t, m)
	for _, w := range []string{`mongorescue_active_runs{kind="backup"} 0`, `mongorescue_active_runs{kind="restore"} 0`, `mongorescue_restores_total{status="cancelled"} 0`} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q before any run", w)
		}
	}
	m.SetActiveRunsSource(func(kind string) int {
		if kind == KindBackup {
			return 2
		}
		return 1
	})
	ctx := context.Background()
	m.ObserveEvent(ctx, events.Event{Type: events.BackupCancelled, JobID: "nightly", Duration: time.Second})
	m.ObserveEvent(ctx, events.Event{Type: events.RestoreCancelled, Duration: 3 * time.Second})
	m.ObserveEvent(ctx, events.Event{Type: events.RestoreSucceeded, Duration: time.Second})

	out = scrape(t, m)
	for _, w := range []string{
		`mongorescue_active_runs{kind="backup"} 2`,
		`mongorescue_active_runs{kind="restore"} 1`,
		`mongorescue_backups_total{job="nightly",status="cancelled"} 1`,
		`mongorescue_restores_total{status="cancelled"} 1`,
		`mongorescue_restore_duration_seconds_count 2`,
		`mongorescue_restore_duration_seconds_sum 4`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in exposition", w)
		}
	}
	if strings.Contains(out, `mongorescue_backups_total{job="nightly",status="failed"}`) {
		t.Error("a cancelled backup must not count as failed")
	}
	m.ForgetJob("nightly")
	if strings.Contains(scrape(t, m), `job="nightly"`) {
		t.Error("ForgetJob must drop the cancelled series too")
	}
	m.SetActiveRunsSource(nil)
	if !strings.Contains(scrape(t, m), `mongorescue_active_runs{kind="backup"} 0`) {
		t.Error("without a source the gauge reports 0")
	}
}
