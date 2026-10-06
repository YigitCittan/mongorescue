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

// TestReportCountsCopiesAndWarnsWhenTheyAreOverdue proves that a row shows the
// copies of its newest good backup, and warns with copy_missing only once they are
// overdue.
func TestReportCountsCopiesAndWarnsWhenTheyAreOverdue(t *testing.T) {
	st := storetest.New(t)
	clk := &clock{now: t0.Add(2 * time.Hour)}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now, CopyMissingAfter: 6 * time.Hour})
	saveJob(t, st, &models.Job{ID: "job_c", Name: "copies", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: "c1"})
	b := &models.BackupRecord{ID: "bc", JobID: "job_c", Database: "shop", ConnectionID: "c1", StartedAt: t0.Add(time.Hour),
		Verification: models.VerificationOK}
	b.PlanCopies([]models.CopyTarget{{ID: "tgt_a"}, {ID: "tgt_b"}}, "")
	b.Copies[0].Status = models.CopyDone
	saveBackup(t, st, b)

	row := func() readiness.Row {
		t.Helper()
		report, err := svc.Report(context.Background())
		if err != nil || len(report.Rows) != 1 {
			t.Fatalf("report = %+v, %v", report, err)
		}
		return report.Rows[0]
	}
	if r := row(); r.Copies != 1 || r.CopyTargets != 2 || slices.Contains(r.Reasons, readiness.ReasonCopyMissing) {
		t.Fatalf("fresh backup: copies %d of %d, reasons %v", r.Copies, r.CopyTargets, r.Reasons)
	}
	clk.set(t0.Add(8 * time.Hour))
	if r := row(); !slices.Contains(r.Reasons, readiness.ReasonCopyMissing) || r.Status == readiness.StatusFail {
		t.Fatalf("overdue copies: status %s, reasons %v", r.Status, r.Reasons)
	}
}

// TestReportWarnsAboutAnOlderBackupWithExhaustedCopies proves that copy_missing
// covers every backup whose copies the queue gave up on, not only the newest.
func TestReportWarnsAboutAnOlderBackupWithExhaustedCopies(t *testing.T) {
	st := storetest.New(t)
	clk := &clock{now: t0.Add(3 * time.Hour)}
	svc := readiness.New(readiness.Config{Store: unedited{st}, Now: clk.Now})
	saveJob(t, st, &models.Job{ID: "job_c", Name: "copies", Database: "shop", CronExpression: "@daily", Enabled: true, ConnectionID: "c1"})
	old := &models.BackupRecord{ID: "b_old", JobID: "job_c", Database: "shop", ConnectionID: "c1", StartedAt: t0}
	old.PlanCopies([]models.CopyTarget{{ID: "tgt_a"}}, "")
	old.Copies[0].Status, old.Copies[0].Attempts = models.CopyFailed, 10
	saveBackup(t, st, old)
	fresh := &models.BackupRecord{ID: "b_new", JobID: "job_c", Database: "shop", ConnectionID: "c1", StartedAt: t0.Add(2 * time.Hour)}
	fresh.PlanCopies([]models.CopyTarget{{ID: "tgt_a"}}, "")
	fresh.Copies[0].Status = models.CopyDone
	saveBackup(t, st, fresh)

	report, err := svc.Report(context.Background())
	if err != nil || len(report.Rows) != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if r := report.Rows[0]; r.Copies != 1 || !slices.Contains(r.Reasons, readiness.ReasonCopyMissing) {
		t.Fatalf("copies %d, reasons %v; want copy_missing for the older backup", r.Copies, r.Reasons)
	}
}
