package readiness_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestStreamWithoutAWindowIsNotOK(t *testing.T) {
	durable := 20.0
	// Healthy collector, but no eligible base: bases fail, or oplog_max_days removed
	// the chunks they need.
	stream := readiness.StreamInfo{ID: "pst_a", ConnectionID: "c1", Enabled: true, Running: true, DurableRPOSeconds: &durable}
	svc := readiness.New(readiness.Config{Store: storetest.New(t),
		Streams: func(context.Context) ([]readiness.StreamInfo, error) { return []readiness.StreamInfo{stream}, nil }})
	report, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := report.Streams[0]
	if row.Status != readiness.StatusWarn || !slices.Contains(row.Reasons, readiness.ReasonPITRNoWindow) {
		t.Fatalf("stream without a window: %+v", row)
	}
}
