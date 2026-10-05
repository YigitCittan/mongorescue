package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
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
	st.Windows = append(st.Windows, collector.Window{ChainID: "cur", Open: true, StartTime: ended, EndTime: ended.Add(time.Hour)})
	if info = streamInfo(st); !info.WindowOpen || !info.WindowEnd.Equal(ended.Add(time.Hour)) {
		t.Fatalf("info with a window on the current chain = %+v", info)
	}
}

func TestCorruptChunkFailsTheReadinessRow(t *testing.T) {
	durable := 30.0
	st := &collector.StreamStatus{
		Stream:            &pitr.Stream{ID: "pst_a", ConnectionID: "c1", Enabled: true},
		State:             &pitr.State{Status: pitr.CollectorRunning},
		Running:           true,
		DurableRPOSeconds: &durable,
		// The sweep found a missing chunk: the window before it is closed.
		Chains:  []collector.ChainStatus{{Chain: pitr.Chain{ChainID: "cur"}, Corrupt: 1}},
		Windows: []collector.Window{{ChainID: "cur", Open: false, StartTime: time.Now().Add(-time.Hour), EndTime: time.Now()}},
	}
	info := streamInfo(st)
	if !info.Broken || info.WindowOpen {
		t.Fatalf("info = %+v", info)
	}
	svc := readiness.New(readiness.Config{Store: storetest.New(t),
		Streams: func(context.Context) ([]readiness.StreamInfo, error) { return []readiness.StreamInfo{info}, nil }})
	report, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Streams) != 1 || report.Streams[0].Status != readiness.StatusFail ||
		!slices.Contains(report.Streams[0].Reasons, readiness.ReasonPITRChainBroken) {
		t.Fatalf("stream row %+v", report.Streams)
	}
}
