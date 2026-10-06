package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// lockedStore is a mock bucket with S3 Object Lock: uploads are locked for a day,
// deleting a key adds nothing (it would only hide the data), and versions are kept
// until their lock ends.
type lockedStore struct {
	*storage.MockStorage
	until   time.Time
	deletes int
}

func (l *lockedStore) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	obj, err := l.MockStorage.Save(ctx, key, r)
	if obj != nil {
		obj.VersionID, obj.RetainUntil, obj.ObjectLockMode = "v1", &l.until, models.ObjectLockCompliance
	}
	return obj, err
}

func (l *lockedStore) Delete(context.Context, string) error {
	l.deletes++
	return nil
}

func (l *lockedStore) ObjectLockEnabled() bool                                  { return true }
func (l *lockedStore) CheckObjectLock(context.Context) error                    { return nil }
func (l *lockedStore) SetLegalHold(context.Context, string, string, bool) error { return nil }
func (l *lockedStore) PurgeVersions(_ context.Context, _ string, now time.Time) (*time.Time, error) {
	if now.Before(l.until) {
		return &l.until, nil
	}
	return nil, nil
}

// TestFailedBackupOnALockedTargetLeavesItsArtifactToThePurge proves that a backup
// failing after its upload to a locked target neither deletes the key (a delete
// marker would hide the locked data for good) nor forgets the artifact: the record
// keeps its version and lock and is marked for the purge.
func TestFailedBackupOnALockedTargetLeavesItsArtifactToThePurge(t *testing.T) {
	st := &lockedStore{MockStorage: storage.NewMockStorage(), until: time.Now().Add(24 * time.Hour).Truncate(time.Second)}
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("partial-data"))), strings.NewReader(""), func() error { return errors.New("exit status 1") }, nil
	}
	rec, err := NewEngine(st, "mongodb://localhost:27017", WithRunner(runner)).Run(context.Background(), models.BackupOptions{Database: "shop"})
	if err == nil || rec.Status != models.StatusFailed {
		t.Fatalf("run = %+v, %v; want a failed backup", rec, err)
	}
	if st.deletes != 0 {
		t.Fatalf("the failed artifact got %d plain deletes; want none", st.deletes)
	}
	if !rec.ArchiveCleanupPending || rec.StorageVersionID != "v1" || rec.RetainUntil == nil || !rec.RetainUntil.Equal(st.until) {
		t.Fatalf("record = pending %v, version %q, retain until %v; want the artifact left to the purge", rec.ArchiveCleanupPending, rec.StorageVersionID, rec.RetainUntil)
	}
}
