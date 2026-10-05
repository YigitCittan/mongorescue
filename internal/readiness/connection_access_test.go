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

// TestReportEvidenceFollowsTheBackupsConnection checks that a job moved from
// another connection does not show that connection's backups to a limited caller,
// and that a restore into a connection the caller may not touch is no RTO evidence.
func TestReportEvidenceFollowsTheBackupsConnection(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	clk := &clock{now: t0.Add(48 * time.Hour)}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now})
	saveJob(t, st, &models.Job{ID: "job_m", Name: "moved", Database: "shop", CronExpression: "@daily", Enabled: true,
		ConnectionID: "c1", CreatedAt: t0})
	saveBackup(t, st, &models.BackupRecord{ID: "b_c1", JobID: "job_m", Database: "shop", ConnectionID: "c1", StartedAt: t0.Add(10 * time.Hour),
		Verification: models.VerificationOK})
	saveBackup(t, st, &models.BackupRecord{ID: "b_c2", JobID: "job_m", Database: "shop", ConnectionID: "c2", StartedAt: t0.Add(20 * time.Hour),
		Verification: models.VerificationOK})
	restored := t0.Add(30 * time.Hour)
	if err := st.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: "rs_c2", SourceDatabase: "shop", SourceConnectionID: "c1",
		TargetConnectionID: "c2", Status: models.RestoreStatusCompleted, StartedAt: t0.Add(29 * time.Hour), CompletedAt: &restored,
		DurationSeconds: 900}); err != nil {
		t.Fatal(err)
	}
	limited := auth.WithPrincipal(ctx, &auth.Principal{Method: auth.MethodSession, Scope: auth.ScopeOperator, Connections: auth.OnlyConnections("c1")})
	report, err := svc.Report(limited)
	if err != nil || len(report.Rows) != 1 {
		t.Fatalf("limited report = %+v, %v", report, err)
	}
	row := report.Rows[0]
	if row.LastGoodBackup == nil || row.LastGoodBackup.ID != "b_c1" || row.LastVerifiedBackup == nil || row.LastVerifiedBackup.ID != "b_c1" {
		t.Fatalf("evidence of a limited caller = good %+v, verified %+v; want b_c1", row.LastGoodBackup, row.LastVerifiedBackup)
	}
	if row.RTO != nil && row.RTO.Source == readiness.RTOSourceRestore {
		t.Fatalf("RTO from a restore into c2: %+v", row.RTO)
	}
	if report, err = svc.Report(ctx); err != nil || report.Rows[0].LastGoodBackup.ID != "b_c2" ||
		report.Rows[0].RTO == nil || report.Rows[0].RTO.Source != readiness.RTOSourceRestore {
		t.Fatalf("unlimited row = %+v, %v", report.Rows[0], err)
	}
}
