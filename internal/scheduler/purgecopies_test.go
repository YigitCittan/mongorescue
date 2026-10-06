package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestPurgeRemovesEveryCopyHonouringEachTargetsLock proves that purging a deleted
// backup deletes its copies on their own targets too, that a copy on a locked
// target keeps the backup deleted until that target's lock ends (even when the
// lock was not recorded), and that copies already deleted are not deleted again.
func TestPurgeRemovesEveryCopyHonouringEachTargetsLock(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	primary, plain := storage.NewMockStorage(), storage.NewMockStorage()
	locked := &lockedBucket{MockStorage: storage.NewMockStorage(), until: map[string]time.Time{"v-bkp_c": t0.Add(30 * 24 * time.Hour)}}
	drivers := map[string]storage.Storage{"tgt_p": primary, "tgt_plain": plain, "tgt_locked": locked}
	storages := func(_ context.Context, id string) (storage.Storage, error) {
		if d, ok := drivers[id]; ok {
			return d, nil
		}
		return nil, errors.New("unknown target")
	}
	saveDeleted(t, st, primary, "bkp_c", t0, testGrace, func(r *models.BackupRecord) {
		r.StorageTargetID = "tgt_p"
		r.PlanCopies([]models.CopyTarget{{ID: "tgt_plain"}, {ID: "tgt_locked"}}, "")
		// The locked copy was recorded without its version and lock.
		r.Copies[0].Status, r.Copies[1].Status = models.CopyDone, models.CopyDone
	})
	for _, d := range []*storage.MockStorage{plain, locked.MockStorage} {
		if _, err := d.Save(ctx, "shop/bkp_c.archive", strings.NewReader("archive")); err != nil {
			t.Fatal(err)
		}
	}
	purgeAt := func(at time.Time) []string {
		t.Helper()
		locked.mu.Lock()
		locked.now = at
		locked.mu.Unlock()
		purged, err := PurgeDeleted(ctx, at, testGrace, st, storages, nil, nil)
		if err != nil {
			t.Fatalf("purge at %s: %v", at, err)
		}
		return purged
	}
	if got := purgeAt(t0.Add(testGrace)); len(got) != 0 {
		t.Fatalf("purged %v while a copy is locked", got)
	}
	rec, _ := st.GetBackupRecord(ctx, "bkp_c")
	if rec.Status != models.StatusDeleted || rec.Copies[0].Status != models.CopyPurged || rec.Copies[1].RetainUntil == nil {
		t.Fatalf("after the first purge: status %s, copies %+v", rec.Status, rec.Copies)
	}
	if _, err := primary.Stat(ctx, rec.StorageKey); err != nil {
		t.Fatalf("the primary must stay while a copy is locked: %v", err)
	}
	if got := purgeAt(t0.Add(30 * 24 * time.Hour)); !slices.Equal(got, []string{"bkp_c"}) {
		t.Fatalf("purge once the copy's lock ended = %v", got)
	}
	rec, _ = st.GetBackupRecord(ctx, "bkp_c")
	if rec.Status != models.StatusPurged || rec.Copies[1].Status != models.CopyPurged || rec.HoldsTarget("tgt_locked") {
		t.Fatalf("after the purge: status %s, copies %+v", rec.Status, rec.Copies)
	}
	for id, d := range map[string]storage.Storage{"tgt_p": primary, "tgt_plain": plain, "tgt_locked": locked} {
		if _, err := d.Stat(ctx, rec.StorageKey); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("the archive on %s must be gone: %v", id, err)
		}
	}
	if len(locked.markers) != 0 {
		t.Fatalf("the locked copy got delete markers %v", locked.markers)
	}
}
