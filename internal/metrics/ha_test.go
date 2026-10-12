package metrics

import (
	"strings"
	"testing"
)

func TestHASeries(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	if out := scrape(t, m); strings.Contains(out, "mongorescue_ha_") {
		t.Fatal("HA series are exported outside HA mode")
	}
	m.SetHASource(func() HAState {
		return HAState{Cluster: "prod", Role: "active", Epoch: 4, LeaseHealthy: true, LeaseErrors: 2, Takeovers: 1, LastTakeoverSeconds: 1.5}
	})
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_ha_role{cluster="prod",role="active"} 1`,
		`mongorescue_ha_role{cluster="prod",role="standby"} 0`,
		`mongorescue_ha_epoch{cluster="prod"} 4`,
		`mongorescue_ha_lease_healthy{cluster="prod"} 1`,
		`mongorescue_ha_lease_errors_total{cluster="prod"} 2`,
		`mongorescue_ha_takeovers_total{cluster="prod"} 1`,
		`mongorescue_ha_takeover_duration_seconds{cluster="prod"} 1.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	m.SetHASource(nil)
	if out = scrape(t, m); strings.Contains(out, "mongorescue_ha_") {
		t.Fatal("HA series are still exported after the source was removed")
	}
}
