package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPITRChainTestUnverified(t *testing.T) {
	m := New(BuildInfo{})
	m.SetPITRChainTestUnverified("str_a", 3)
	m.SetPITRChainTestUnverified("str_b", 0)
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_pitr_chain_test_unverified_collections{stream="str_a"} 3`,
		`mongorescue_pitr_chain_test_unverified_collections{stream="str_b"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	m.ForgetPITRStream("str_a")
	if strings.Contains(scrape(t, m), `chain_test_unverified_collections{stream="str_a"}`) {
		t.Error("the series of a deleted stream must be removed")
	}
}

func TestPITRBaseWindowRatio(t *testing.T) {
	m := New(BuildInfo{})
	m.SetPITRBaseWindowRatio("str_a", 0.62, true)
	m.SetPITRBaseWindowRatio("str_b", 0.1, true)
	out := scrape(t, m)
	for _, want := range []string{
		`mongorescue_pitr_base_window_ratio{stream="str_a"} 0.62`,
		`mongorescue_pitr_base_window_ratio{stream="str_b"} 0.1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	// A standby exports no PITR series at all.
	rec := httptest.NewRecorder()
	m.StandbyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "mongorescue_pitr_base_window_ratio") {
		t.Error("a standby exports the base window ratio")
	}
	// No ratio known removes the series; a deleted stream's series goes too.
	m.SetPITRBaseWindowRatio("str_b", 0, false)
	m.ForgetPITRStream("str_a")
	if strings.Contains(scrape(t, m), "mongorescue_pitr_base_window_ratio") {
		t.Error("the series must be removed")
	}
}
