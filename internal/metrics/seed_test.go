package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestSeedLastBackups(t *testing.T) {
	m := New(BuildInfo{Version: "1", Commit: "c", GoVersion: "go"})
	at := time.Unix(1_790_000_000, 0).UTC()
	m.SeedLastBackups([]BackupSeed{
		{JobID: "nightly", Database: "shop", At: at.Add(-time.Hour), SizeBytes: 10},
		{JobID: "nightly", Database: "crm", At: at, SizeBytes: 20},
		{JobID: "", Database: "x", At: at, SizeBytes: 1},
	})
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_last_successful_backup_timestamp_seconds{job="nightly"} 1.79e+09`,
		`mongorescue_backup_size_bytes{job="nightly"} 20`,
		`mongorescue_database_last_successful_backup_timestamp_seconds{database="shop",job="nightly"} 1.7899964e+09`,
		`mongorescue_database_backup_size_bytes{database="crm",job="nightly"} 20`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape lacks %s\n%s", want, out)
		}
	}
	if strings.Contains(out, `database="x"`) {
		t.Error("a seed without a job produced a series")
	}
	m.ForgetJob("nightly")
	if out := scrape(t, m); strings.Contains(out, `job="nightly"} 20`) {
		t.Error("ForgetJob left seeded series")
	}
}
