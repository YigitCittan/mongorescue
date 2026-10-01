package integrity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestImportRevivesTheRecordOfItsKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pruned := f.putBackup(t, "bkp_pruned", "job_1", f.now.Add(-96*time.Hour), []byte("old archive"), func(r *models.BackupRecord) {
		r.Status = models.StatusPruned
	})
	rec, err := f.svc.StartImport(ctx, "tgt_local", pruned.StorageKey)
	if err != nil || rec.ID != pruned.ID || rec.Status != models.StatusPending || !rec.Imported || rec.Trigger != models.TriggerManual {
		t.Fatalf("revived = %+v, %v", rec, err)
	}
	f.waitIdle(t)
	got := f.get(t, pruned.ID)
	if got.Status != models.StatusCompleted || got.SHA256 != pruned.SHA256 || got.Verification != "" {
		t.Fatalf("after import = %+v", got)
	}
	records, _ := f.st.ListBackupRecords(ctx, "")
	if len(records) != 1 {
		t.Fatalf("an import of a known key must not add a record: %d records", len(records))
	}
	if _, err = f.svc.StartImport(ctx, "tgt_local", pruned.StorageKey); !errors.Is(err, ErrNotOrphan) {
		t.Fatalf("importing a live record's key = %v", err)
	}

	// A failed record whose archive changed since it was written is revived and flagged.
	failed := f.putBackup(t, "bkp_failed", "", f.now.Add(-time.Hour), []byte("written"), func(r *models.BackupRecord) {
		r.Status = models.StatusFailed
	})
	f.putObject(t, failed.StorageKey, "changed")
	if _, err = f.svc.StartImport(ctx, "tgt_local", failed.StorageKey); err != nil {
		t.Fatal(err)
	}
	f.waitIdle(t)
	if got := f.get(t, failed.ID); got.Status != models.StatusCompleted || got.Verification != models.VerificationMismatch {
		t.Fatalf("changed archive = %+v", got)
	}
}

func TestImportRefusesKeysWithoutAValidDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, key := range []string{
		"sh*op/2026/09/bkp_shop_20260901_030000_abcd.archive.gz",
		`sh\op/2026/09/bkp_shop_20260901_030000_abcd.archive.gz`,
		"a.b/2026/09/bkp_shop_20260901_030000_abcd.archive",
		"flat.archive",
		"-db/custom.archive",
	} {
		f.putObject(t, key, "x")
		if _, err := f.svc.StartImport(ctx, "tgt_local", key); !errors.Is(err, ErrInvalidImport) {
			t.Errorf("import of %q = %v, want ErrInvalidImport", key, err)
		}
	}
	if records, _ := f.st.ListBackupRecords(ctx, ""); len(records) != 0 {
		t.Fatalf("refused imports created %d records", len(records))
	}
}
