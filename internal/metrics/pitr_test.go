package metrics

import (
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
