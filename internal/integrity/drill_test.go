package integrity

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// drillFixture turns job_1 of the restore test fixture into a DR drill reading
// from copy target tgt_dr, and gives bkp_1 a copy there with the given status.
func drillFixture(t *testing.T, status models.CopyStatus) (*fixture, *models.Job, *models.BackupRecord) {
	t.Helper()
	f, job, rec := restoreTestFixture(t)
	f.targets.targets["tgt_dr"] = &models.StorageTarget{ID: "tgt_dr", Name: "DR", Type: models.StorageLocal, Region: "region-b"}
	f.targets.drivers["tgt_dr"] = f.mem
	job.CopyTargets = []string{"tgt_dr"}
	job.RestoreTest.SourceTargetID = "tgt_dr"
	if err := f.st.UpdateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	updated, err := f.st.UpdateBackupRecord(context.Background(), rec.ID, func(r *models.BackupRecord) error {
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_dr", Name: "DR"}}, "")
		c := &r.Copies[0]
		c.Status, c.SHA256OK, c.SHA256 = status, status == models.CopyDone, r.SHA256
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, job, updated
}

// TestDrillReadsTheCopyTarget proves that a restore test with a source target
// restores from the copy there and records it on the result.
func TestDrillReadsTheCopyTarget(t *testing.T) {
	f, job, rec := drillFixture(t, models.CopyDone)
	f.svc.AfterBackup(context.Background(), job, rec)
	if !slices.Equal(f.restorer.sources, []string{"tgt_dr"}) {
		t.Fatalf("restores read %v; want the copy target", f.restorer.sources)
	}
	list, err := f.st.ListRestoreTests(context.Background(), job.ID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("restore tests = %+v, %v", list, err)
	}
	if r := list[0]; r.Status != models.RestoreTestOK || r.SourceTargetID != "tgt_dr" || r.SourceTargetName != "DR" || r.BackupID != rec.ID {
		t.Fatalf("drill result = %+v", r)
	}
}

// TestDrillWaitsForACompleteCopy proves that a scheduled drill is skipped while no
// backup has a complete copy on the drill's target, that a manual one is refused
// with ErrNoBackup, and that a drill falls back to an older backup whose copy is
// complete.
func TestDrillWaitsForACompleteCopy(t *testing.T) {
	f, job, rec := drillFixture(t, models.CopyPending)
	f.svc.AfterBackup(context.Background(), job, rec)
	if len(f.restorer.sources) != 0 {
		t.Fatalf("a drill ran without a complete copy: %v", f.restorer.sources)
	}
	if list, _ := f.st.ListRestoreTests(context.Background(), job.ID, 10); len(list) != 0 {
		t.Fatalf("a skipped drill was recorded: %+v", list)
	}
	if _, err := f.svc.StartRestoreTest(context.Background(), job.ID); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("manual drill without a copy = %v; want ErrNoBackup", err)
	}

	newer := f.putBackup(t, "bkp_2", job.ID, f.now.Add(-time.Minute), []byte("newer"), func(r *models.BackupRecord) {
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_dr", Name: "DR"}}, "")
	})
	if _, err := f.st.UpdateBackupRecord(context.Background(), rec.ID, func(r *models.BackupRecord) error {
		r.Copies[0].Status, r.Copies[0].SHA256OK = models.CopyDone, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source, err := f.svc.drillSource(context.Background(), job, newer)
	if err != nil || source.ID != rec.ID || source.StorageTargetID != "tgt_dr" {
		t.Fatalf("drill source = %+v, %v; want the copy of the older backup", source, err)
	}
}
