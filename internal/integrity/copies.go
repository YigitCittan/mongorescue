package integrity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/verify"
)

// copyTally counts the outcomes of the copy checks of one backup.
type copyTally struct{ ok, mismatch, errors int }

// verifyCopies re-reads every completed copy of rec (see models.BackupCopy) and
// compares it with the backup's checksum, like the primary archive. The outcome is
// recorded on the copy. A copy that is damaged or gone is queued again (pending):
// the copy queue copies it from the primary once more, and backup.copy_failed is
// published. It returns the updated record and the tally.
func (s *Service) verifyCopies(ctx context.Context, rec *models.BackupRecord, bytesPerSecond int64) (*models.BackupRecord, copyTally) {
	var tally copyTally
	out := rec
	for i := range rec.Copies {
		c := rec.Copies[i]
		if c.Status != models.CopyDone {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		var res verify.Result
		driver, err := s.cfg.Targets.Storage(ctx, c.TargetID)
		if err != nil {
			res = verify.Result{Status: models.VerificationError, At: s.now(), Err: fmt.Errorf("storage target %s: %w", c.TargetID, err)}
		} else {
			res = verify.Archive(ctx, driver, rec.AtCopy(&c), verify.Options{Decryptor: s.verifyDecryptor(), BytesPerSecond: bytesPerSecond})
		}
		if ctx.Err() != nil && res.Status != models.VerificationOK {
			break
		}
		gone := res.Status == models.VerificationMismatch || errors.Is(res.Err, storage.ErrNotFound)
		var updatedCopy models.BackupCopy
		updated, err := s.cfg.Store.UpdateBackupRecord(context.WithoutCancel(ctx), rec.ID, func(r *models.BackupRecord) error {
			cp := r.Copy(c.TargetID)
			if cp == nil || cp.Status != models.CopyDone || cp.StorageKey != c.StorageKey {
				return errRecordChanged
			}
			at := res.At
			cp.VerifiedAt, cp.Verification, cp.VerificationError = &at, res.Status, ""
			if res.Err != nil {
				cp.VerificationError = redact.Text(res.Err.Error())
			}
			if gone {
				// Copied again from the primary by the copy queue.
				cp.Status, cp.SHA256OK, cp.NextAttemptAt, cp.Attempts = models.CopyPending, false, nil, 0
				cp.Error = "copied again: " + cp.VerificationError
			}
			updatedCopy = *cp
			return nil
		})
		if err != nil {
			continue
		}
		out = updated
		attrs := []any{slog.String("backup_id", rec.ID), slog.String("storage_target_id", c.TargetID), slog.String("result", string(res.Status))}
		switch res.Status {
		case models.VerificationOK:
			tally.ok++
			s.logger.Info("backup copy verified", attrs...)
			continue
		case models.VerificationMismatch:
			tally.mismatch++
		default:
			tally.errors++
		}
		s.logger.Warn("backup copy verification failed", append(attrs, slog.String("error", updatedCopy.VerificationError))...)
		if gone {
			s.publish(ctx, events.Event{Type: events.BackupCopyFailed, Time: res.At.UTC(), JobID: rec.JobID, BackupID: rec.ID,
				Database: rec.Database, Status: string(updatedCopy.Status), TargetID: c.TargetID, TargetName: c.TargetName,
				Verification: string(res.Status), Error: updatedCopy.VerificationError, Detail: "the copy is copied again from the primary"})
		}
	}
	return out, tally
}
