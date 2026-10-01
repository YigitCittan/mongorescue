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

// TestRetentionNeverDeletesWhatAnUnreadableRowNames checks that a backup row the
// store cannot decode (skipped by every list) is never pruned, and that an archive it
// shares with a pruned backup is kept.
func TestRetentionNeverDeletesWhatAnUnreadableRowNames(t *testing.T) {
	st, mock := storetest.New(t), storage.NewMockStorage()
	records := seedScheduled(t, st, mock, 4, nil)
	// A twin of bkp_02's archive that no longer decodes.
	if err := st.SaveBackupRecord(context.Background(), &models.BackupRecord{ID: "bkp_twin", Database: "shop",
		Status: models.StatusCompleted, StorageKey: "shop/02.archive", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	storetest.CorruptRow(t, st, "backups", "bkp_twin", `{"id":"bkp_twin","storage_key":"shop/02.archive","status":7}`)
	// bkp_03 no longer decodes either: the store skips it.
	storetest.CorruptRow(t, st, "backups", "bkp_03", `{"id":"bkp_03","job_id":"job_r","storage_key":"shop/03.archive","status":7}`)
	listed, err := st.ListBackupRecords(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneBackups(context.Background(), 0, 1, listed, st, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(pruned)
	if !slices.Equal(pruned, []string{"bkp_01", "bkp_02"}) {
		t.Fatalf("pruned = %v (the plan from the stale list was %d records)", pruned, len(records))
	}
	for _, key := range []string{"shop/02.archive", "shop/03.archive"} {
		if _, err := mock.Stat(context.Background(), key); err != nil {
			t.Errorf("%s must stay (named by an unreadable row): %v", key, err)
		}
	}
	if _, err := mock.Stat(context.Background(), "shop/01.archive"); err == nil {
		t.Error("an unshared archive must be deleted")
	}
}
