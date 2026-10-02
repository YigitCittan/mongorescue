package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestRPOGauges(t *testing.T) {
	m := New(BuildInfo{Version: "1", Commit: "c", GoVersion: "go"})
	now := time.Unix(1_790_000_000, 0).UTC()
	m.rpo.now = func() time.Time { return now }
	m.SetRPOSamples(now, []RPOSample{
		{JobID: "hourly", Database: "shop", Since: now.Add(-2 * time.Hour), Target: 6 * time.Hour},
		{JobID: "hourly", Database: "crm", Since: now.Add(-7 * time.Hour), Target: 6 * time.Hour},
		{JobID: "daily", Database: "shop", Since: now.Add(-time.Hour), Target: 49 * time.Hour},
	})
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_job_rpo_seconds{database="shop",job="hourly"} 7200`,
		`mongorescue_job_rpo_seconds{database="crm",job="hourly"} 25200`,
		`mongorescue_job_rpo_met{database="shop",job="hourly"} 1`,
		`mongorescue_job_rpo_met{database="crm",job="hourly"} 0`,
		`mongorescue_job_rpo_target_seconds{database="shop",job="daily"} 176400`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape lacks %s", want)
		}
	}

	// A deleted job and a replaced set of samples leave no series behind.
	m.ForgetJob("hourly")
	if out = scrape(t, m); strings.Contains(out, `job="hourly"`) || !strings.Contains(out, `job="daily"`) {
		t.Errorf("after ForgetJob:\n%s", out)
	}
	// A check that started before the deletion cannot bring the job back; one that
	// started after it can (an ID used again).
	stale := []RPOSample{{JobID: "hourly", Database: "shop", Since: now, Target: time.Hour}, {JobID: "daily", Database: "shop", Since: now, Target: time.Hour}}
	m.SetRPOSamples(now.Add(-time.Second), stale)
	if out = scrape(t, m); strings.Contains(out, `job="hourly"`) || !strings.Contains(out, `job="daily"`) {
		t.Errorf("a check from before ForgetJob resurrected the job:\n%s", out)
	}
	m.rpo.now = func() time.Time { return now.Add(time.Minute) }
	m.SetRPOSamples(now.Add(time.Second), stale)
	if out = scrape(t, m); !strings.Contains(out, `job="hourly"`) {
		t.Errorf("a check from after ForgetJob lost the job:\n%s", out)
	}
	m.SetRPOSamples(now.Add(time.Second), nil)
	if out = scrape(t, m); strings.Contains(out, "mongorescue_job_rpo_seconds{") {
		t.Errorf("after an empty check:\n%s", out)
	}
}
