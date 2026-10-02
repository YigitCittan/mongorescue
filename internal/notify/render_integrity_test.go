package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

func TestRenderIntegrityEvents(t *testing.T) {
	at := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		e    events.Event
		want []string
	}{
		{events.Event{Type: events.VerificationFailed, BackupID: "bkp_1", Database: "shop", Source: "sweep", Verification: "mismatch", Error: "sha256 differs", Time: at},
			[]string{"Backup verification failed: backup bkp_1 (db shop, sweep check: mismatch)", "sha256 differs"}},
		{events.Event{Type: events.RestoreTestFailed, JobID: "nightly", Database: "shop", BackupID: "bkp_2", Error: "orders: 3 documents restored", Time: at},
			[]string{"Restore test failed: job nightly (db shop), backup bkp_2", "orders: 3 documents"}},
		{events.Event{Type: events.RestoreTestSucceeded, JobID: "nightly", Database: "shop", BackupID: "bkp_2", Time: at},
			[]string{"Restore test passed"}},
		{events.Event{Type: events.RestoreVerificationFailed, BackupID: "bkp_3", RestoreID: "rst_1", Database: "shop_rescue", Error: "1 mismatch(es) with the backup's manifest",
			Detail: "collection orders: 3 documents restored, 4 expected", Time: at},
			[]string{"Restore verification failed: backup bkp_3 → db shop_rescue", "1 mismatch(es)", "Detail: collection orders"}},
		{events.Event{Type: events.DriftDetected, TargetName: "S3 prod", Orphans: 2, Missing: 1, Detail: "2 orphan archive(s), 1 missing archive(s)", Time: at},
			[]string{"Storage drift detected: storage target S3 prod: 2 orphan archive(s), 1 missing archive(s)", "Detail: 2 orphan"}},
		{events.Event{Type: events.RetentionDeleted, JobID: "nightly", Database: "shop", BackupID: "bkp_0", Detail: "older than 30 days", Time: at},
			[]string{"Backup deleted by retention: job nightly (db shop), backup bkp_0", "Detail: older than 30 days"}},
	} {
		msg := Render(tc.e)
		for _, want := range tc.want {
			if !strings.Contains(msg.Body, want) {
				t.Errorf("%s: body %q lacks %q", tc.e.Type, msg.Body, want)
			}
		}
		if strings.ContainsAny(msg.Subject, "\r\n") {
			t.Errorf("%s: subject spans lines", tc.e.Type)
		}
	}
	p := NewWebhookPayload(Render(events.Event{Type: events.DriftDetected, TargetID: "tgt", Orphans: 4, Missing: 2, Source: "manual"}))
	if p.TargetID != "tgt" || p.Orphans != 4 || p.Missing != 2 || p.Source != "manual" {
		t.Fatalf("webhook payload = %+v", p)
	}
}
