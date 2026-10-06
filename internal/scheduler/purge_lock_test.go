package scheduler

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// lockedBucket is a versioned mock bucket with S3 Object Lock: deleting a key only
// hides it (a delete marker), and deleting a version fails before its retain-until
// date at the bucket's clock.
type lockedBucket struct {
	*storage.MockStorage
	mu      sync.Mutex
	now     time.Time
	until   map[string]time.Time
	markers []string
	deleted []string
}

func (b *lockedBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.markers = append(b.markers, key)
	return nil
}

func (b *lockedBucket) RetrieveVersion(ctx context.Context, key, _ string) (io.ReadCloser, error) {
	return b.Retrieve(ctx, key)
}

func (b *lockedBucket) ObjectLockEnabled() bool                                  { return true }
func (b *lockedBucket) CheckObjectLock(context.Context) error                    { return nil }
func (b *lockedBucket) SetLegalHold(context.Context, string, string, bool) error { return nil }

// PurgeVersions deletes the key's one version ("v-" + the backup ID) once its lock
// ended at now, and refuses an early delete like S3 would.
func (b *lockedBucket) PurgeVersions(ctx context.Context, key string, now time.Time) (*time.Time, error) {
	b.mu.Lock()
	versionID := "v-" + strings.TrimSuffix(strings.TrimPrefix(key, "shop/"), ".archive")
	if until, ok := b.until[versionID]; ok && now.Before(until) {
		b.mu.Unlock()
		return &until, nil
	}
	if b.now.Before(b.until[versionID]) {
		b.mu.Unlock()
		return nil, errors.New("AccessDenied: object is WORM protected")
	}
	b.deleted = append(b.deleted, key+"@"+versionID)
	b.mu.Unlock()
	return nil, b.MockStorage.Delete(ctx, key)
}

// TestPurgeWaitsForTheObjectLockAndDeletesTheVersion proves that a deleted backup
// whose archive is under an S3 Object Lock retention stays deleted (not purged)
// until the retention ends, even after its grace period, and that the purge then
// deletes the recorded version, not just the key, so the space is freed.
func TestPurgeWaitsForTheObjectLockAndDeletesTheVersion(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	bucket := &lockedBucket{MockStorage: storage.NewMockStorage(), until: map[string]time.Time{}}
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// bkp_long is locked for 30 days, longer than the grace period; bkp_short for
	// one day (retention_days=1), shorter than it.
	for id, days := range map[string]int{"bkp_long": 30, "bkp_short": 1} {
		until := t0.Add(time.Duration(days) * 24 * time.Hour)
		bucket.until["v-"+id] = until
		saveDeleted(t, st, bucket.MockStorage, id, t0, testGrace, func(r *models.BackupRecord) {
			r.StorageVersionID, r.RetainUntil, r.ObjectLockMode = "v-"+r.ID, &until, models.ObjectLockCompliance
		})
	}

	purgeAt := func(at time.Time) []string {
		t.Helper()
		bucket.mu.Lock()
		bucket.now = at
		bucket.mu.Unlock()
		purged, err := PurgeDeleted(ctx, at, testGrace, st, fixedStorage(bucket), nil, nil)
		if err != nil {
			t.Fatalf("purge at %s: %v", at, err)
		}
		return purged
	}
	if got := purgeAt(t0.Add(2 * 24 * time.Hour)); len(got) != 0 {
		t.Fatalf("purged %v inside the grace period", got)
	}
	if got := purgeAt(t0.Add(testGrace)); !slices.Equal(got, []string{"bkp_short"}) {
		t.Fatalf("purge after the grace period = %v; want only bkp_short (bkp_long is still locked)", got)
	}
	rec, err := st.GetBackupRecord(ctx, "bkp_long")
	if err != nil || rec.Status != models.StatusDeleted || !rec.LockedAt(t0.Add(testGrace)) {
		t.Fatalf("bkp_long = %+v, %v; want deleted and locked", rec, err)
	}
	if got := purgeAt(t0.Add(30*24*time.Hour - time.Second)); len(got) != 0 {
		t.Fatalf("purged %v a second before the lock ends", got)
	}
	if got := purgeAt(t0.Add(30 * 24 * time.Hour)); !slices.Equal(got, []string{"bkp_long"}) {
		t.Fatalf("purge once the lock ended = %v; want bkp_long", got)
	}
	want := []string{"shop/bkp_short.archive@v-bkp_short", "shop/bkp_long.archive@v-bkp_long"}
	if !slices.Equal(bucket.deleted, want) || len(bucket.markers) != 0 {
		t.Fatalf("deleted versions %v, delete markers %v; want versions %v and no markers", bucket.deleted, bucket.markers, want)
	}
	for _, id := range []string{"bkp_long", "bkp_short"} {
		if rec, err = st.GetBackupRecord(ctx, id); err != nil || rec.Status != models.StatusPurged {
			t.Errorf("%s = %+v, %v; want purged", id, rec, err)
		}
	}
}

// TestPurgeFindsTheLockOfARecordWithoutOne proves that on a locked target a deleted
// backup recorded without a version or lock (an import, a backup taken before the
// lock) is not marked purged while a version is still locked: the purge reads the
// versions, records when the lock ends and purges the backup only afterwards.
func TestPurgeFindsTheLockOfARecordWithoutOne(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	until := t0.Add(20 * 24 * time.Hour)
	bucket := &lockedBucket{MockStorage: storage.NewMockStorage(), until: map[string]time.Time{"v-bkp_import": until}, now: t0.Add(testGrace)}
	saveDeleted(t, st, bucket.MockStorage, "bkp_import", t0, testGrace, nil)

	if got, err := PurgeDeleted(ctx, t0.Add(testGrace), testGrace, st, fixedStorage(bucket), nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("purge while a version is locked = %v, %v; want nothing purged", got, err)
	}
	rec, err := st.GetBackupRecord(ctx, "bkp_import")
	if err != nil || rec.Status != models.StatusDeleted || rec.RetainUntil == nil || !rec.RetainUntil.Equal(until) {
		t.Fatalf("record = %+v, %v; want deleted with the lock found in storage", rec, err)
	}
	bucket.now = until
	if got, err := PurgeDeleted(ctx, until, testGrace, st, fixedStorage(bucket), nil, nil); err != nil || !slices.Equal(got, []string{"bkp_import"}) {
		t.Fatalf("purge once the lock ended = %v, %v", got, err)
	}
	if !slices.Equal(bucket.deleted, []string{"shop/bkp_import.archive@v-bkp_import"}) {
		t.Fatalf("deleted versions = %v", bucket.deleted)
	}
}

// TestPurgeDeletesTheLockedArtifactOfAFailedBackup proves that the artifact a failed
// backup left on a locked target (ArchiveCleanupPending) is deleted, version and
// all, once its lock ends, and that the flag is cleared then and not before.
func TestPurgeDeletesTheLockedArtifactOfAFailedBackup(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	until := t0.Add(24 * time.Hour)
	bucket := &lockedBucket{MockStorage: storage.NewMockStorage(), until: map[string]time.Time{"v-bkp_fail": until}, now: t0}
	rec := &models.BackupRecord{ID: "bkp_fail", JobID: "job_p", Database: "shop", Status: models.StatusFailed, StartedAt: t0,
		StorageKey: "shop/bkp_fail.archive", StorageVersionID: "v-bkp_fail", RetainUntil: &until, ArchiveCleanupPending: true}
	if err := st.SaveBackupRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := bucket.MockStorage.Save(ctx, rec.StorageKey, strings.NewReader("partial")); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{t0, until} {
		bucket.now = at
		if _, err := PurgeDeleted(ctx, at, testGrace, st, fixedStorage(bucket), nil, nil); err != nil {
			t.Fatalf("purge at %s: %v", at, err)
		}
		got, err := st.GetBackupRecord(ctx, "bkp_fail")
		if err != nil {
			t.Fatal(err)
		}
		if locked := at.Before(until); got.ArchiveCleanupPending != locked || got.Status != models.StatusFailed {
			t.Fatalf("at %s: pending %v, status %s; want pending=%v and still failed", at, got.ArchiveCleanupPending, got.Status, locked)
		}
	}
	if !slices.Equal(bucket.deleted, []string{"shop/bkp_fail.archive@v-bkp_fail"}) {
		t.Fatalf("deleted versions = %v", bucket.deleted)
	}
}
