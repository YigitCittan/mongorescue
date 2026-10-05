package scheduler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestRetentionRechecksLastVerifiedAtDeletion(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 3, nil)
	// bkp_02 passed verification after retention listed the records: the stale
	// copies say unverified, so the plan selects it, but it is now the newest
	// verified backup and must stay.
	if _, err := st.UpdateBackupRecord(context.Background(), "bkp_02", func(r *models.BackupRecord) error {
		r.Verification = models.VerificationOK
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneBackups(context.Background(), 0, 1, records, st, 7*24*time.Hour, nil)
	if err != nil || !slices.Equal(pruned, []string{"bkp_01"}) {
		t.Fatalf("pruned = %v, %v", pruned, err)
	}
	if _, err := mock.Stat(context.Background(), "shop/02.archive"); err != nil {
		t.Fatalf("the newest verified backup's archive must stay: %v", err)
	}
}

func TestRetentionKeepsSharedArchives(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 3, nil)
	// A manual record (e.g. an import) names the archive of bkp_02 too.
	if err := st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "bkp_twin", Database: "shop", Trigger: models.TriggerManual,
		Status: models.StatusCompleted, StorageKey: "shop/02.archive", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	grace := 7 * 24 * time.Hour
	var entries []models.RetentionLogEntry
	_, err := prune(context.Background(), now, grace, 0, 1, records, st, nil,
		func(_ context.Context, e models.RetentionLogEntry) { entries = append(entries, e) })
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	for _, e := range entries {
		if e.PurgeAfter == nil || !e.PurgeAfter.Equal(now.Add(grace)) {
			t.Fatalf("the log must say until when the backup is recoverable: %+v", e)
		}
	}
	// Retention deletes softly: both archives stay for the grace period.
	for _, key := range []string{"shop/01.archive", "shop/02.archive"} {
		if _, statErr := mock.Stat(context.Background(), key); statErr != nil {
			t.Fatalf("retention must not delete %s before the grace period: %v", key, statErr)
		}
	}
	storages := func(context.Context, string) (storage.Storage, error) { return mock, nil }
	var kept []PurgeOutcome
	purged, err := PurgeDeleted(context.Background(), now.Add(grace), grace, st, storages, nil,
		func(_ context.Context, o PurgeOutcome) { kept = append(kept, o) })
	if err != nil || len(purged) != 2 {
		t.Fatalf("purged = %v, %v", purged, err)
	}
	if _, err := mock.Stat(context.Background(), "shop/02.archive"); err != nil {
		t.Fatalf("an archive another record names must stay: %v", err)
	}
	if _, err := mock.Stat(context.Background(), "shop/01.archive"); err == nil {
		t.Fatal("an unshared archive must be deleted by the purge")
	}
	for _, o := range kept {
		if o.Backup.ID == "bkp_02" && (o.ArchiveKept != "bkp_twin" || o.ArchiveDeleted) {
			t.Fatalf("the purge must say why the archive was kept: %+v", o)
		}
		if o.Backup.Status != models.StatusPurged {
			t.Fatalf("%s = %s; want purged", o.Backup.ID, o.Backup.Status)
		}
	}
}
