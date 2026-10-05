package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestLivenessSeries(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	out := scrape(t, m)
	for _, w := range []string{"mongorescue_scheduler_last_tick_timestamp_seconds 0", "mongorescue_settings_warnings 0"} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape without sources lacks %s", w)
		}
	}
	m.SetSchedulerTickSource(func() time.Time { return time.Unix(1_700_000_000, 0) })
	m.SetSettingsWarningsSource(func() int { return 2 })
	out = scrape(t, m)
	for _, w := range []string{"mongorescue_scheduler_last_tick_timestamp_seconds 1.7e+09", "mongorescue_settings_warnings 2"} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape lacks %s", w)
		}
	}
	m.SetSchedulerTickSource(func() time.Time { return time.Time{} })
	m.SetSettingsWarningsSource(nil)
	out = scrape(t, m)
	if !strings.Contains(out, "mongorescue_scheduler_last_tick_timestamp_seconds 0") || !strings.Contains(out, "mongorescue_settings_warnings 0") {
		t.Errorf("a zero tick and a removed source must report 0:\n%s", out)
	}
}

func TestJobFailureCountersStartAtZero(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	now := time.Now()
	m.SetRPOSamples(now, []RPOSample{{JobID: "nightly", Database: "shop", Since: now, Target: time.Hour}})
	out := scrape(t, m)
	for _, w := range []string{
		`mongorescue_backups_total{job="nightly",status="failed"} 0`,
		`mongorescue_backups_total{job="manual",status="failed"} 0`,
		`mongorescue_job_runs_total{job="nightly",status="partial"} 0`,
		`mongorescue_job_runs_total{job="nightly",status="failed"} 0`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape lacks %s", w)
		}
	}
	m.ForgetJob("nightly")
	if out = scrape(t, m); strings.Contains(out, `job="nightly"`) {
		t.Errorf("a deleted job's counters linger:\n%s", out)
	}
}
