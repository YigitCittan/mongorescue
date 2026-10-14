package operations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestAPIKeyExpiryIsAnnounced(t *testing.T) {
	sink := &alertSink{}
	env := newProtEnv(t)
	env.cfg.Alerts = sink
	env.svc = operations.New(env.cfg)
	deadline := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	soon := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	env.svc.APIKeyExpiryChecked(context.Background(), &auth.ExpiryCheck{
		Deadline: deadline, MaxLifetime: 90 * 24 * time.Hour,
		Capped:   []*auth.APIKey{{ID: "key_1", Name: "ci", Prefix: "abcdefgh", ExpiresAt: &deadline}},
		Announce: []*auth.APIKey{{ID: "key_2", Name: "grafana", Prefix: "ijklmnop", ExpiresAt: &soon}},
	})
	env.svc.APIKeyExpiryChecked(context.Background(), nil)

	env.pub.mu.Lock()
	byType := map[events.EventType][]events.Event{}
	for _, e := range env.pub.events {
		byType[e.Type] = append(byType[e.Type], e)
	}
	env.pub.mu.Unlock()
	set, expiring := byType[events.APIKeyDeadlineSet], byType[events.APIKeyExpiring]
	if len(set) != 1 || len(expiring) != 1 || len(sink.events()) != 1 {
		t.Fatalf("deadline_set %d, expiring %d, stored alerts %d; want 1 each", len(set), len(expiring), len(sink.events()))
	}
	if d := set[0].Detail; !strings.Contains(d, "is 90") || !strings.Contains(d, `"ci" (mr_abcdefgh_…)`) || !strings.Contains(d, "2027-01-01T00:00:00Z") {
		t.Errorf("deadline_set detail = %q", d)
	}
	if d := expiring[0].Detail; !strings.Contains(d, `"grafana"`) || !strings.Contains(d, "key_2") || !strings.Contains(d, "2026-10-20T00:00:00Z") {
		t.Errorf("expiring detail = %q", d)
	}
	for _, typ := range []events.EventType{events.APIKeyDeadlineSet, events.APIKeyExpiring} {
		if !typ.Subscribable() || typ.Failed() || typ.Broadcast() {
			t.Errorf("%s: subscribable %v, failed %v, broadcast %v", typ, typ.Subscribable(), typ.Failed(), typ.Broadcast())
		}
	}
}
