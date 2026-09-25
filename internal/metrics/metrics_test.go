package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// scrape returns the text exposition of m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestMetricsFromEvents(t *testing.T) {
	m := New(BuildInfo{Version: "1.2.3", Commit: "abc", GoVersion: "go1.24"})
	m.SetScheduledJobsSource(func() int { return 3 })

	ctx := context.Background()
	ts := time.Unix(1_790_000_000, 0).UTC()
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "nightly", Time: ts, SizeBytes: 4096, Duration: 2 * time.Second})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "nightly", Duration: time.Second})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, SizeBytes: 10})
	m.ObserveEvent(ctx, events.Event{Type: events.RestoreFailed})
	m.ObserveEvent(ctx, events.Event{Type: events.NotificationTest})
	m.ObserveNotification("webhook", "success")
	m.IncEventsDropped(events.Event{})

	out := scrape(t, m)
	want := []string{
		`mongorescue_backups_total{job="nightly",status="succeeded"} 1`,
		`mongorescue_backups_total{job="nightly",status="failed"} 1`,
		`mongorescue_backups_total{job="manual",status="succeeded"} 1`,
		`mongorescue_backup_duration_seconds_count{job="nightly"} 2`,
		`mongorescue_backup_size_bytes{job="nightly"} 4096`,
		`mongorescue_last_successful_backup_timestamp_seconds{job="nightly"} 1.79e+09`,
		`mongorescue_restores_total{status="failed"} 1`,
		`mongorescue_restores_total{status="succeeded"} 0`,
		`mongorescue_notifications_total{channel_type="webhook",status="success"} 1`,
		`mongorescue_events_dropped_total 1`,
		`mongorescue_scheduled_jobs 3`,
		`mongorescue_build_info{commit="abc",go_version="go1.24",version="1.2.3"} 1`,
		`go_goroutines`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in exposition", w)
		}
	}
	if strings.Contains(out, `mongorescue_backup_size_bytes{job="nightly"} 0`) {
		t.Error("a failed backup must not reset the last successful size")
	}
}

func TestForgetJob(t *testing.T) {
	m := New(BuildInfo{})
	ctx := context.Background()
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "gone", SizeBytes: 1, Time: time.Now()})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupFailed, JobID: "gone"})
	m.ObserveEvent(ctx, events.Event{Type: events.BackupSucceeded, JobID: "kept"})

	m.ForgetJob("gone")
	m.ForgetJob("")
	out := scrape(t, m)
	if strings.Contains(out, `job="gone"`) {
		t.Error("series of a deleted job must be removed")
	}
	if !strings.Contains(out, `mongorescue_backups_total{job="kept",status="succeeded"} 1`) {
		t.Error("other jobs must be unaffected")
	}
}

func TestScheduledJobsWithoutSource(t *testing.T) {
	m := New(BuildInfo{})
	if out := scrape(t, m); !strings.Contains(out, "mongorescue_scheduled_jobs 0") {
		t.Error("scheduled_jobs should default to 0")
	}
	m.SetScheduledJobsSource(func() int { return 1 })
	m.SetScheduledJobsSource(nil)
	if out := scrape(t, m); !strings.Contains(out, "mongorescue_scheduled_jobs 0") {
		t.Error("clearing the source should report 0")
	}
}

func TestObserveMCPCall(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	m.ObserveMCPCall("start_backup", "ok")
	m.ObserveMCPCall("start_backup", "ok")
	m.ObserveMCPCall("start_backup", "denied")
	out := scrape(t, m)
	for _, w := range []string{
		`mongorescue_mcp_calls_total{result="ok",tool="start_backup"} 2`,
		`mongorescue_mcp_calls_total{result="denied",tool="start_backup"} 1`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape lacks %s", w)
		}
	}
}
