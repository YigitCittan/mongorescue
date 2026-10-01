package scheduler

import (
	"context"
	"slices"
	"strings"
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
	pruned, err := PruneBackups(context.Background(), 0, 1, records, st, mock, nil)
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
	var entries []models.RetentionLogEntry
	_, err := prune(context.Background(), time.Now().UTC(), 0, 1, records, st,
		func(context.Context, string) (storage.Storage, error) { return mock, nil }, nil,
		func(_ context.Context, e models.RetentionLogEntry) { entries = append(entries, e) })
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if _, err := mock.Stat(context.Background(), "shop/02.archive"); err != nil {
		t.Fatalf("an archive another record names must stay: %v", err)
	}
	if _, err := mock.Stat(context.Background(), "shop/01.archive"); err == nil {
		t.Fatal("an unshared archive must be deleted")
	}
	for _, e := range entries {
		if e.BackupID == "bkp_02" && !strings.Contains(e.Detail, "bkp_twin") {
			t.Fatalf("the log must say why the archive was kept: %+v", e)
		}
	}
}
