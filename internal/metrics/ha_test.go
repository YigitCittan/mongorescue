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
		return HAState{Cluster: "prod", Role: "active", Epoch: 4, LeaseHealthy: true, LeaseErrors: 2, Takeovers: 1, LastTakeoverSeconds: 1.5, StepDowns: 3}
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
		`mongorescue_ha_step_downs_total{cluster="prod"} 3`,
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

// The swap series of in-place PITR belong to the active instance: a standby
// exports none of them.
func TestHASwapSeriesAreActiveOnly(t *testing.T) {
	m := New(BuildInfo{Version: "test"})
	m.SetHASource(func() HAState {
		return HAState{Cluster: "prod", Role: "active", Epoch: 4, SwapsHeld: 1, SwapHandovers: map[string]uint64{"completed": 2, "timeout": 1}}
	})
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_ha_pitr_swaps_held{cluster="prod"} 1`,
		`mongorescue_ha_pitr_swap_handovers_total{cluster="prod",outcome="completed"} 2`,
		`mongorescue_ha_pitr_swap_handovers_total{cluster="prod",outcome="failed"} 0`,
		`mongorescue_ha_pitr_swap_handovers_total{cluster="prod",outcome="timeout"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("active: missing %q", want)
		}
	}
	m.SetHASource(func() HAState { return HAState{Cluster: "prod", Role: "standby", SwapsHeld: 1} })
	if out = scrape(t, m); strings.Contains(out, "pitr_swap") {
		t.Fatalf("a standby exports swap series:\n%s", out)
	}
}
