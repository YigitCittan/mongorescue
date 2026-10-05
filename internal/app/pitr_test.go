package app

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
)

func TestStreamInfoForReadiness(t *testing.T) {
	ended := time.Now()
	lag := time.Now()
	st := &collector.StreamStatus{
		Stream:  &pitr.Stream{ID: "pst_a", ConnectionID: "c1", Enabled: true},
		State:   &pitr.State{Status: pitr.CollectorFailed, LagSince: &lag},
		Running: true,
		Live:    &collector.Live{WindowLow: true},
		Chains: []collector.ChainStatus{
			{Chain: pitr.Chain{ChainID: "old", EndReason: pitr.EndGap, EndedAt: &ended}},
			{Chain: pitr.Chain{ChainID: "cur"}},
		},
		Windows: []collector.Window{{ChainID: "old", StartTime: ended.Add(-time.Hour), EndTime: ended}},
	}
	info := streamInfo(st)
	if !info.Failing || !info.LagHigh || !info.WindowLow || !info.Broken || info.WindowOpen || info.WindowEnd == nil {
		t.Fatalf("info = %+v", info)
	}
	st.Windows = append(st.Windows, collector.Window{ChainID: "cur", StartTime: ended, EndTime: ended.Add(time.Hour)})
	if info = streamInfo(st); !info.WindowOpen || !info.WindowEnd.Equal(ended.Add(time.Hour)) {
		t.Fatalf("info with a window on the current chain = %+v", info)
	}
}
