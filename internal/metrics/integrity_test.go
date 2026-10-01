package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

func TestIntegrityMetrics(t *testing.T) {
	m := New(BuildInfo{Version: "v", Commit: "c", GoVersion: "go"})
	ctx := context.Background()
	ts := time.Unix(1_790_000_000, 0).UTC()
	m.ObserveEvent(ctx, events.Event{Type: events.VerificationFailed, Source: events.VerificationSweep, Verification: "mismatch"})
	m.ObserveEvent(ctx, events.Event{Type: events.VerificationSucceeded, Source: events.VerificationAfterUpload, Verification: "ok"})
	m.ObserveEvent(ctx, events.Event{Type: events.VerificationFailed, Source: "bogus", Verification: "ok"}) // unknown labels are dropped
	m.ObserveEvent(ctx, events.Event{Type: events.RestoreTestSucceeded, JobID: "nightly", Verification: "ok", Time: ts})
	m.ObserveEvent(ctx, events.Event{Type: events.RestoreTestFailed, JobID: "nightly", Verification: "weird"})
	m.ObserveEvent(ctx, events.Event{Type: events.RetentionDeleted, JobID: "nightly"})
	m.ObserveStorageScan("tgt_1", 3, 1, ts)
	m.ObserveStorageScan("", 9, 9, ts)

	body := scrape(t, m)
	for _, want := range []string{
		`mongorescue_verifications_total{result="mismatch",source="sweep"} 1`,
		`mongorescue_verifications_total{result="ok",source="after_upload"} 1`,
		`mongorescue_verifications_total{result="error",source="on_demand"} 0`,
		`mongorescue_restore_tests_total{job="nightly",result="ok"} 1`,
		`mongorescue_restore_tests_total{job="nightly",result="error"} 1`,
		`mongorescue_last_successful_restore_test_timestamp_seconds{job="nightly"} 1.79e+09`,
		`mongorescue_retention_deletions_total{job="nightly"} 1`,
		`mongorescue_storage_orphan_archives{target="tgt_1"} 3`,
		`mongorescue_storage_missing_archives{target="tgt_1"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `source="bogus"`) {
		t.Error("unknown sources must not create series")
	}

	m.ForgetJob("nightly")
	m.ForgetTarget("tgt_1")
	body = scrape(t, m)
	if strings.Contains(body, `job="nightly"`) || strings.Contains(body, `target="tgt_1"`) {
		t.Errorf("forgotten series linger:\n%s", body)
	}
}
