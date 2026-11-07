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

// TestReportWarnsAboutABackupThroughAMongos proves that a newest good backup that
// carries the sharded_unsupported warning makes its row warn with that reason, and
// that a newer backup without it clears the reason.
func TestReportWarnsAboutABackupThroughAMongos(t *testing.T) {
	st := storetest.New(t)
	clk := &clock{now: t0.Add(3 * time.Hour)}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now})
	saveJob(t, st, &models.Job{ID: "job_s", Name: "sharded", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: "c1"})
	saveBackup(t, st, &models.BackupRecord{ID: "b1", JobID: "job_s", Database: "shop", ConnectionID: "c1", StartedAt: t0.Add(time.Hour),
		Warnings: []string{models.ShardedWarning}})

	row := func() readiness.Row {
		t.Helper()
		report, err := svc.Report(context.Background())
		if err != nil || len(report.Rows) != 1 {
			t.Fatalf("report = %+v, %v", report, err)
		}
		return report.Rows[0]
	}
	if r := row(); !slices.Contains(r.Reasons, readiness.ReasonShardedUnsupported) || r.Status == readiness.StatusOK {
		t.Fatalf("status %s, reasons %v; want sharded_unsupported", r.Status, r.Reasons)
	}
	saveBackup(t, st, &models.BackupRecord{ID: "b2", JobID: "job_s", Database: "shop", ConnectionID: "c1", StartedAt: t0.Add(2 * time.Hour)})
	if r := row(); slices.Contains(r.Reasons, readiness.ReasonShardedUnsupported) {
		t.Fatalf("reasons %v after a backup without the warning", r.Reasons)
	}
}
