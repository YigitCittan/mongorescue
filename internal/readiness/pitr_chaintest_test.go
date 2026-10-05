package readiness

import (
	"slices"
	"testing"
)

func TestFailedChainTestWarns(t *testing.T) {
	st := StreamInfo{Enabled: true, Running: true, WindowOpen: true, ChainTestFailed: true}
	fail, warn := streamReasons(st)
	if len(fail) != 0 || !slices.Contains(warn, ReasonPITRChainTestFailed) {
		t.Fatalf("fail %v, warn %v", fail, warn)
	}
	st.ChainTestFailed = false
	if _, warn = streamReasons(st); slices.Contains(warn, ReasonPITRChainTestFailed) {
		t.Fatal("a passed chain test warns")
	}
}
