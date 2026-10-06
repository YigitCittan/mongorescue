package reencrypt_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// lockedDriver is a mock bucket with S3 Object Lock: every upload gets version
// "v-<key>" and a retention; reads by version are recorded, plain deletes are
// refused (they would only add a delete marker), and legal holds are recorded.
type lockedDriver struct {
	*storage.MockStorage
	until time.Time
	// failNew makes reads of the re-encrypted archive fail, so it is discarded.
	failNew bool

	mu       sync.Mutex
	versions []string
	holds    []string
	deletes  int
}

func (d *lockedDriver) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	obj, err := d.MockStorage.Save(ctx, key, r)
	if obj != nil {
		obj.VersionID, obj.RetainUntil, obj.ObjectLockMode = "v-"+key, &d.until, models.ObjectLockCompliance
	}
	return obj, err
}

func (d *lockedDriver) Retrieve(ctx context.Context, key string) (io.ReadCloser, error) {
	if d.failNew && strings.Contains(key, "-rk") {
		return io.NopCloser(bytes.NewReader([]byte("garbage"))), nil
	}
	return d.MockStorage.Retrieve(ctx, key)
}

func (d *lockedDriver) RetrieveVersion(ctx context.Context, key, versionID string) (io.ReadCloser, error) {
	d.mu.Lock()
	d.versions = append(d.versions, versionID)
	d.mu.Unlock()
	return d.Retrieve(ctx, key)
}

func (d *lockedDriver) Delete(context.Context, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes++
	return nil
}

func (d *lockedDriver) ObjectLockEnabled() bool               { return true }
func (d *lockedDriver) CheckObjectLock(context.Context) error { return nil }
func (d *lockedDriver) SetLegalHold(_ context.Context, key, versionID string, on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if on {
		d.holds = append(d.holds, key+"@"+versionID)
	}
	return nil
}

// PurgeVersions keeps every object until the lock ends.
func (d *lockedDriver) PurgeVersions(_ context.Context, _ string, now time.Time) (*time.Time, error) {
	if now.Before(d.until) {
		return &d.until, nil
	}
	return nil, nil
}

// TestReencryptOnALockedTarget proves that on a target with S3 Object Lock the old
// archive is read by its recorded version, the new one is uploaded locked and its
// version and lock recorded on the backup, a pinned backup's legal hold moves to
// the new archive, and the tombstone keeps the old version, lock and hold for the
// purge, which deletes it only once the lock ends.
func TestReencryptOnALockedTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, k := storetest.New(t), newKeys(t)
	drv := &lockedDriver{MockStorage: storage.NewMockStorage(), until: time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)}
	old := seedBackup(t, st, drv.MockStorage, k.oldEnc, "bkp_a")
	oldUntil := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	old.StorageVersionID, old.RetainUntil, old.ObjectLockMode = "v-old", &oldUntil, models.ObjectLockCompliance
	old.Pinned, old.LegalHold = true, true
	if err := st.SaveBackupRecord(ctx, old); err != nil {
		t.Fatal(err)
	}
	svc := service(st, drv, k, decryptor(t, k.newID, k.oldID), nil)
	svc.Start(ctx)
	if _, err := svc.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if job := waitDone(t, svc); job.Status != reencrypt.StatusCompleted || job.Done != 1 {
		t.Fatalf("job = %+v", job)
	}
	rec, err := st.GetBackupRecord(ctx, "bkp_a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.StorageVersionID != "v-"+rec.StorageKey || rec.RetainUntil == nil || !rec.RetainUntil.Equal(drv.until) || !rec.LegalHold || !rec.Pinned {
		t.Fatalf("backup = version %q, until %v, hold %v; want the new locked, held archive", rec.StorageVersionID, rec.RetainUntil, rec.LegalHold)
	}
	if len(drv.versions) == 0 || drv.versions[0] != "v-old" {
		t.Fatalf("version reads = %v; want the old archive read by its version", drv.versions)
	}
	if len(drv.holds) != 1 || drv.holds[0] != rec.StorageKey+"@v-"+rec.StorageKey {
		t.Fatalf("legal holds = %v; want one on the new archive's version", drv.holds)
	}
	list, _ := st.ListBackupRecords(ctx, "")
	var tomb *models.BackupRecord
	for _, r := range list {
		if r.ID != "bkp_a" {
			tomb = r
		}
	}
	if tomb == nil || tomb.StorageKey != old.StorageKey || tomb.StorageVersionID != "v-old" || tomb.RetainUntil == nil ||
		!tomb.RetainUntil.Equal(oldUntil) || !tomb.LegalHold || tomb.Pinned {
		t.Fatalf("tombstone = %+v; want the old version, its lock and its hold", tomb)
	}
	if tomb.PurgeDue(oldUntil.Add(-time.Second), 0) || !tomb.PurgeDue(oldUntil, 0) {
		t.Fatal("the tombstone must be purged only once the old archive's lock ended")
	}
	if drv.deletes != 0 {
		t.Fatalf("%d plain deletes on a locked target; want none", drv.deletes)
	}
}

// TestReencryptHandsALockedDiscardToThePurge proves that a re-encrypted archive
// that fails its check is not deleted with a plain delete on a locked target (it is
// locked already) but recorded as a deleted record the purge removes once the lock
// ends.
func TestReencryptHandsALockedDiscardToThePurge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, k := storetest.New(t), newKeys(t)
	drv := &lockedDriver{MockStorage: storage.NewMockStorage(), until: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second), failNew: true}
	seedBackup(t, st, drv.MockStorage, k.oldEnc, "bkp_a")
	svc := service(st, drv, k, decryptor(t, k.newID, k.oldID), nil)
	svc.Start(ctx)
	if _, err := svc.Trigger(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if job := waitDone(t, svc); job.Failed != 1 {
		t.Fatalf("job = %+v; want the backup failed", job)
	}
	list, _ := st.ListBackupRecords(ctx, "")
	var part *models.BackupRecord
	for _, r := range list {
		if strings.Contains(r.ID, "-reenc-partial-") {
			part = r
		}
	}
	if part == nil || part.Status != models.StatusDeleted || !strings.Contains(part.StorageKey, "-rk") ||
		part.RetainUntil == nil || !part.RetainUntil.Equal(drv.until) || part.StorageVersionID != "v-"+part.StorageKey {
		t.Fatalf("discarded archive record = %+v; want a deleted record with its version and lock", part)
	}
	if drv.deletes != 0 {
		t.Fatalf("%d plain deletes on a locked target; want none", drv.deletes)
	}
}
