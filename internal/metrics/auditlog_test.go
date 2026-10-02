package metrics

import (
	"strings"
	"testing"
)

func TestObserveAuditForward(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	if out := scrape(t, m); !strings.Contains(out, `mongorescue_audit_forward_total{outcome="dropped"} 0`) {
		t.Fatal("the dropped series is not pre-created")
	}
	m.ObserveAuditForward("sent")
	m.ObserveAuditForward("sent")
	m.ObserveAuditForward("dropped")
	m.ObserveAuditForward("bogus")
	out := scrape(t, m)
	for _, w := range []string{
		`mongorescue_audit_forward_total{outcome="sent"} 2`,
		`mongorescue_audit_forward_total{outcome="failed"} 0`,
		`mongorescue_audit_forward_total{outcome="dropped"} 1`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape lacks %s", w)
		}
	}
	if strings.Contains(out, "bogus") {
		t.Error("an unknown outcome created a series")
	}
}
