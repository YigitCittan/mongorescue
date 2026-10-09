package operations

import (
	"context"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// finishRestore stores final, the record of a finished restore, and only then
// publishes its outcome, so restore.succeeded is never sent for a restore the
// metadata does not record. A record that cannot be saved (after the retries of
// diskguard.Guard.SaveFinal, which also detects a full data directory) makes the
// restore fail (see failUnsavedRestore). withVerification also publishes the
// outcome of the restore's verification.
func (s *Service) finishRestore(runCtx context.Context, final *models.RestoreRecord, runErr error, backupID string, withVerification bool) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
	defer cancel()
	saveErr := s.cfg.DiskGuard.SaveFinal(persistCtx, func(ctx context.Context) error {
		return s.store.SaveRestoreRecord(ctx, final)
	})
	if saveErr != nil {
		s.failUnsavedRestore(persistCtx, final, saveErr, runs.FromContext(runCtx))
	}
	s.publish(persistCtx, events.RestoreEvent(final, runErr, backupID))
	if saveErr != nil || !withVerification {
		return
	}
	if ve, ok := events.RestoreVerificationEvent(final); ok {
		s.publish(persistCtx, ve)
	}
}

// failUnsavedRestore settles rec, a restore whose final record could not be saved
// (saveErr), as failed: what it restored is handled like the clones of any failed
// restore (a safe clone, or the clones of a point-in-time restore or chain test,
// are dropped; an in-place restore's target is never touched), the failure is
// logged and written to the run's log, and saving the failed record is retried
// once writes work again.
func (s *Service) failUnsavedRestore(ctx context.Context, rec *models.RestoreRecord, saveErr error, run *runs.Run) {
	outcome := rec.Status
	note := ""
	if outcome == models.RestoreStatusCompleted && !rec.DryRun {
		switch {
		case rec.PITR != nil:
			note = s.CleanupInterruptedPITR(ctx, rec)
		case !rec.InPlace:
			note = s.dropUnsavedClone(ctx, rec)
		}
	}
	cause := redact.Text(saveErr.Error())
	rec.FailUnsaved(cause, note, time.Now())
	s.logger.Error("the final record of a restore could not be saved; the restore is reported as failed",
		logsafe.Attr("restore_id", rec.ID), slog.String("outcome", string(outcome)), logsafe.Error(saveErr))
	run.Printf("ERROR: the restore record could not be saved (%s); the restore is reported as failed%s", cause, note)
	failed := *rec
	s.cfg.DiskGuard.Defer("restore "+rec.ID, func(ctx context.Context) error { return s.store.SaveRestoreRecord(ctx, &failed) })
}

// dropUnsavedClone drops the safe clone of rec, a completed restore whose record
// could not be saved, and returns a note for its error message.
func (s *Service) dropUnsavedClone(ctx context.Context, rec *models.RestoreRecord) string {
	name := rec.TargetDatabase
	if s.cfg.Dropper == nil || name == "" {
		return "; the clone " + name + " was kept, drop it manually"
	}
	conn, err := s.ResolveConnection(ctx, rec.TargetConnectionID)
	if err == nil {
		err = s.cfg.Dropper.DropDatabase(mongotls.NewContext(ctx, conn.TLS()), conn.URI, name)
	}
	if err != nil {
		s.logger.Warn("failed to drop the safe clone of a restore whose record could not be saved",
			logsafe.Attr("restore_id", rec.ID), logsafe.Attr("database", name), logsafe.Error(err))
		return "; dropping the clone " + name + " failed (" + redact.Text(err.Error()) + "), drop it manually"
	}
	return "; the clone " + name + " was dropped"
}
