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
	m.SetRPOSamples([]RPOSample{
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
	m.SetRPOSamples(nil)
	if out = scrape(t, m); strings.Contains(out, "mongorescue_job_rpo_seconds{") {
		t.Errorf("after an empty check:\n%s", out)
	}
}
