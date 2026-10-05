package readiness_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestReportUsesThePITRStream(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(48 * time.Hour)}
	durable := 90.0
	stream := readiness.StreamInfo{ID: "pst_a", ConnectionID: "c1", ReplicaSet: "rs0", Enabled: true, Running: true,
		DurableRPOSeconds: &durable, WindowOpen: true}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now,
		Streams: func(context.Context) ([]readiness.StreamInfo, error) { return []readiness.StreamInfo{stream}, nil }})

	// A daily job whose backup is two days old: its RPO is missed, but the stream
	// recovers the database to 90 seconds ago.
	saveJob(t, st, &models.Job{ID: "job_a", Name: "shop", Database: "shop", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", RPOMinutes: 60, CreatedAt: t0})
	saveBackup(t, st, &models.BackupRecord{ID: "b1", JobID: "job_a", Database: "shop", ConnectionID: "c1", StartedAt: t0,
		Verification: models.VerificationOK})

	report, err := svc.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 || len(report.Streams) != 1 {
		t.Fatalf("%d rows, %d streams", len(report.Rows), len(report.Streams))
	}
	row := report.Rows[0]
	if row.RPO.Source != readiness.RPOSourcePITR || *row.RPO.AgeSeconds != 90 || row.RPO.Met == nil || !*row.RPO.Met ||
		slices.Contains(row.Reasons, readiness.ReasonRPOMissed) || row.RPO.JobAgeSeconds == nil {
		t.Fatalf("row RPO %+v, reasons %v", row.RPO, row.Reasons)
	}
	if report.Streams[0].Status != readiness.StatusOK {
		t.Fatalf("stream %+v", report.Streams[0])
	}

	// A broken chain without a base and a failing collector fail the row; the job
	// RPO counts again.
	stream.Failing, stream.Broken, stream.WindowOpen, stream.LagHigh, stream.WindowLow = true, true, false, true, true
	if report, err = svc.Report(ctx); err != nil {
		t.Fatal(err)
	}
	row = report.Rows[0]
	for _, r := range []string{readiness.ReasonRPOMissed, readiness.ReasonPITRCollectorDown, readiness.ReasonPITRChainBroken,
		readiness.ReasonPITRLagHigh, readiness.ReasonPITRWindowLow} {
		if !slices.Contains(row.Reasons, r) {
			t.Errorf("reasons %v lack %s", row.Reasons, r)
		}
	}
	if row.Status != readiness.StatusFail || row.RPO.Source != readiness.RPOSourceJob || report.Streams[0].Status != readiness.StatusFail {
		t.Fatalf("row %s (%s), stream %s", row.Status, row.RPO.Source, report.Streams[0].Status)
	}

	// A disabled stream changes nothing.
	stream = readiness.StreamInfo{ID: "pst_a", ConnectionID: "c1"}
	if report, err = svc.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if r := report.Rows[0]; r.RPO.PITRAgeSeconds != nil || slices.Contains(r.Reasons, readiness.ReasonPITRCollectorDown) {
		t.Fatalf("disabled stream row %+v", r)
	}
}
