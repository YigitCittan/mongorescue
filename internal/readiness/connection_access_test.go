package readiness_test

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestReportFollowsConnectionAccess checks that a caller limited to one connection
// gets the database rows and the PITR streams of that connection only.
func TestReportFollowsConnectionAccess(t *testing.T) {
	st := storetest.New(t)
	clk := &clock{now: t0.Add(time.Hour)}
	streams := []readiness.StreamInfo{{ID: "pst_a", ConnectionID: "c1", ReplicaSet: "rs0"}, {ID: "pst_b", ConnectionID: "c2", ReplicaSet: "rs1"}}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now,
		Streams: func(context.Context) ([]readiness.StreamInfo, error) { return streams, nil }})
	for _, c := range []string{"c1", "c2"} {
		saveJob(t, st, &models.Job{ID: "job_" + c, Name: c, Database: "db_" + c, CronExpression: "@daily", Enabled: true,
			ConnectionID: c, CreatedAt: t0})
	}
	limited := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeOperator,
		Connections: auth.OnlyConnections("c1")})
	report, err := svc.Report(limited)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 || report.Rows[0].ConnectionID != "c1" || len(report.Streams) != 1 || report.Streams[0].ID != "pst_a" {
		t.Fatalf("limited report: rows %+v, streams %+v", report.Rows, report.Streams)
	}
	if report, err = svc.Report(context.Background()); err != nil || len(report.Rows) != 2 || len(report.Streams) != 2 {
		t.Fatalf("unlimited report: %+v, %v", report, err)
	}
}
