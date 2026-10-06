package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// ErrArchiveChanged is returned by SwapBackupArchive when the backup no longer has
// the archive that was re-encrypted (it was deleted, moved or swapped meanwhile).
var ErrArchiveChanged = errors.New("store: the backup's archive changed meanwhile")

// ArchiveSwap describes a re-encrypted archive for SwapBackupArchive.
type ArchiveSwap struct {
	// BackupID is the backup; OldKey the archive it must still reference.
	BackupID, OldKey string
	// NewKey, SizeBytes, SHA256 and EncryptionMode describe the new archive.
	NewKey         string
	SizeBytes      int64
	SHA256         string
	EncryptionMode string
	// Object is the new archive as stored: its S3 version and Object Lock
	// retention on a locked target (nil or empty elsewhere).
	Object *models.StorageObject
	// Tombstone is a deleted record (StatusDeleted, PurgeAfter set) that keeps the
	// old archive until the delete grace period ends; the purge then removes it.
	Tombstone *models.BackupRecord
}

// SwapBackupArchive points backup sw.BackupID at its re-encrypted archive and stores
// sw.Tombstone, which hands the old archive to the delete grace period, in one
// transaction. It returns ErrNotFound for an unknown backup and ErrArchiveChanged
// when the backup was deleted or no longer references sw.OldKey.
func (s *SQLiteStore) SwapBackupArchive(ctx context.Context, sw ArchiveSwap) error {
	if sw.BackupID == "" || sw.NewKey == "" || sw.Tombstone == nil || sw.Tombstone.ID == "" {
		return fmt.Errorf("%w: archive swap needs a backup, a new key and a tombstone", ErrInvalidRecord)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		rec, err := getRecord[models.BackupRecord](ctx, tx, ErrNotFound, "SELECT data FROM backups WHERE id = ?", sw.BackupID)
		if err != nil {
			return err
		}
		if rec.StorageKey != sw.OldKey || rec.Status.Deleted() {
			return ErrArchiveChanged
		}
		rec.StorageKey, rec.SizeBytes, rec.SHA256, rec.EncryptionMode = sw.NewKey, sw.SizeBytes, sw.SHA256, sw.EncryptionMode
		// The version and lock now describe the new archive; the tombstone keeps
		// those of the old one.
		rec.StorageVersionID, rec.RetainUntil, rec.ObjectLockMode = "", nil, ""
		rec.SetStorageObject(sw.Object)
		if err = putBackup(ctx, tx, rec); err != nil {
			return err
		}
		return putBackup(ctx, tx, sw.Tombstone)
	})
}
