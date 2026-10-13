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

func TestUnverifiedChainTestWarns(t *testing.T) {
	st := StreamInfo{Enabled: true, Running: true, WindowOpen: true,
		ChainTest: &ChainTestInfo{Verification: "passed", Unverified: 2, OldBase: true}}
	fail, warn := streamReasons(st)
	if len(fail) != 0 || !slices.Equal(warn, []string{ReasonPITRChainTestUnverified}) {
		t.Fatalf("fail %v, warn %v", fail, warn)
	}
	st.ChainTest.Unverified = 0
	if _, warn = streamReasons(st); len(warn) != 0 {
		t.Fatalf("a fully verified chain test warns: %v", warn)
	}
	// A failed chain test says so, not that counts were unverified.
	st.ChainTestFailed, st.ChainTest.Unverified = true, 1
	if _, warn = streamReasons(st); !slices.Equal(warn, []string{ReasonPITRChainTestFailed}) {
		t.Fatalf("warn %v", warn)
	}
}
