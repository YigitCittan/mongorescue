package operations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestTLSCertExpiryIsAnnounced(t *testing.T) {
	env := newProtEnv(t)
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	env.svc.TLSCertExpiryChecked(context.Background(), []connections.TLSCertStatus{
		{ConnectionID: "conn_1", ConnectionName: "prod", Kind: "client", NotAfter: at, Level: models.TLSExpiryWarn},
		{ConnectionID: "conn_1", ConnectionName: "prod", Kind: "client", NotAfter: at, Level: models.TLSExpiryCritical},
		{ConnectionID: "conn_2", ConnectionName: "lab", Kind: "ca", NotAfter: at, Level: models.TLSExpiryExpired},
	})
	env.svc.TLSCertExpiryChecked(context.Background(), nil)

	env.pub.mu.Lock()
	got := append([]events.Event(nil), env.pub.events...)
	env.pub.mu.Unlock()
	var expiring, expired []events.Event
	for _, e := range got {
		switch e.Type {
		case events.ConnectionTLSCertExpiring:
			expiring = append(expiring, e)
		case events.ConnectionTLSCertExpired:
			expired = append(expired, e)
		}
	}
	if len(expiring) != 2 || len(expired) != 1 {
		t.Fatalf("expiring %d, expired %d; want 2 and 1", len(expiring), len(expired))
	}
	if e := expiring[1]; e.ConnectionID != "conn_1" || e.Action != "client" ||
		!strings.Contains(e.Detail, `"prod"`) || !strings.Contains(e.Detail, "2026-12-01T00:00:00Z") || !strings.Contains(e.Detail, "7 days") {
		t.Errorf("expiring event = %+v", e)
	}
	if e := expired[0]; e.ConnectionID != "conn_2" || e.Action != "ca" || !strings.Contains(e.Detail, "expired") {
		t.Errorf("expired event = %+v", e)
	}
	for _, typ := range []events.EventType{events.ConnectionTLSCertExpiring, events.ConnectionTLSCertExpired} {
		if !typ.Subscribable() || typ.Failed() || typ.Broadcast() {
			t.Errorf("%s: subscribable %v, failed %v, broadcast %v", typ, typ.Subscribable(), typ.Failed(), typ.Broadcast())
		}
	}
}
