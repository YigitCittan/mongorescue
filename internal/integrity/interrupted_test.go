package integrity

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// The archive of a backup that the startup recovery failed because its record
// was never saved (failed, archive kept, no size or checksum recorded) is
// reported by the storage scan as an orphan and can be imported, which revives
// the record as a completed backup.
func TestScanOffersTheKeptArchiveOfAnInterruptedBackup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.putBackup(t, "bkp_interrupted", "job_1", f.now.Add(-2*time.Hour), []byte("a complete archive"), func(r *models.BackupRecord) {
		r.Status, r.ErrorMessage = models.StatusFailed, "interrupted before the record was saved; the archive was kept"
		r.SizeBytes, r.SHA256, r.ArchiveCleanupPending = 0, "", false
	})
	report, err := f.svc.ScanTarget(ctx, "tgt_local", TriggerManual)
	if err != nil || report.Error != "" {
		t.Fatalf("scan = %+v, %v", report, err)
	}
	if report.OrphanCount != 1 || len(report.Orphans) != 1 || report.Orphans[0].Key != rec.StorageKey ||
		report.Orphans[0].RecordID != rec.ID || report.Orphans[0].RecordStatus != string(models.StatusFailed) {
		t.Fatalf("orphans = %+v; want the kept archive of %s", report.Orphans, rec.ID)
	}
	if _, err = f.svc.StartImport(ctx, "tgt_local", rec.StorageKey); err != nil {
		t.Fatalf("import of the kept archive: %v", err)
	}
	f.waitIdle(t)
	if got := f.get(t, rec.ID); got.Status != models.StatusCompleted || got.SizeBytes != int64(len("a complete archive")) || got.SHA256 == "" {
		t.Fatalf("imported = %+v", got)
	}
}
