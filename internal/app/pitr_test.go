package app

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
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

// TestPITRRTOForReadiness checks the RTO estimate of a stream's readiness row: it
// restores the newest eligible base of the current chain and the oplog chunks
// after the base's t_before, at the rates the estimator gives.
func TestPITRRTOForReadiness(t *testing.T) {
	ctx := context.Background()
	repo := storetest.New(t)
	if err := repo.CreateStream(ctx, &pitr.Stream{ID: "pst_a", ConnectionID: "c1", ReplicaSet: "rs0", TargetID: "tgt",
		BaseCron: "@daily", BaseKeepCount: 7, BaseKeepDays: 14, ChunkSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartChain(ctx, "pst_a", "cur", pitr.OpTime{TS: pitr.Timestamp{T: 100}, Term: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Chunks (100,160], (160,220], (220,280] of 1, 2 and 4 KiB.
	for i, size := range []int64{1 << 10, 2 << 10, 4 << 10} {
		from, to := pitr.Timestamp{T: uint32(100 + 60*i)}, pitr.Timestamp{T: uint32(160 + 60*i)} //nolint:gosec // small test values
		if err := repo.CommitChunk(ctx, &pitr.Chunk{ID: fmt.Sprintf("chk_%d", i), StreamID: "pst_a", ChainID: "cur", TargetID: "tgt",
			StorageKey: fmt.Sprintf("_mongorescue/oplog/c1/rs0/cur/%d.bson.gz.age", i), From: from, To: to, FirstTerm: 1, LastTerm: 1,
			Entries: 10, SizeBytes: size, SHA256: "ab12", Encrypted: true, EncryptionMode: "x25519"}); err != nil {
			t.Fatal(err)
		}
	}
	op := func(sec uint32) *pitr.OpTime { return &pitr.OpTime{TS: pitr.Timestamp{T: sec}, Term: 1} }
	st := &collector.StreamStatus{
		Stream: &pitr.Stream{ID: "pst_a", ConnectionID: "c1", Enabled: true},
		Chains: []collector.ChainStatus{{Chain: pitr.Chain{ChainID: "cur"}}},
		Bases: []collector.BaseStatus{
			{ID: "old", SizeBytes: 1 << 20, TBefore: op(110), TAfter: op(120), ChainID: "cur", Eligible: true},
			// The newest eligible base: the chunks ending after 170 count.
			{ID: "new", SizeBytes: 3 << 20, TBefore: op(170), TAfter: op(175), ChainID: "cur", Eligible: true},
			{ID: "uncovered", SizeBytes: 9 << 20, TBefore: op(250), TAfter: op(300), ChainID: "cur"},
		},
	}
	var gotBase, gotOplog, gotEntries int64
	estimate := func(_ context.Context, _ string, base, oplog, entries int64) models.PITREstimate {
		gotBase, gotOplog, gotEntries = base, oplog, entries
		return models.PITREstimate{Seconds: 42, Source: models.PITREstimateMeasured, BaseBytes: base, OplogBytes: oplog}
	}
	rto := pitrRTO(ctx, repo, estimate, st)
	if rto == nil || rto.Seconds != 42 || gotBase != 3<<20 || gotOplog != 2<<10+4<<10 || gotEntries != 20 {
		t.Fatalf("rto = %+v for base %d, oplog %d and %d entries", rto, gotBase, gotOplog, gotEntries)
	}
	// Without an eligible base there is no estimate.
	st.Bases = st.Bases[2:]
	if rto = pitrRTO(ctx, repo, estimate, st); rto != nil {
		t.Fatalf("rto without an eligible base = %+v", rto)
	}
}
