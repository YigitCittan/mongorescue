package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrPITRUnavailable is returned by StartBaseBackup when no PITR repository is
// configured.
var ErrPITRUnavailable = errors.New("point-in-time recovery is not available")

// StartBaseBackup takes a PITR base backup of stream streamID now, in the
// background: mongodump --oplog of the whole instance of the stream's connection to
// the stream's storage target, recording T_before and T_after (see
// models.ScopeInstance). It runs under the concurrency key
// runs.PITRBaseKey(connection), so the per-database backups of the connection keep
// running, and it returns a snapshot of the in-progress record. trigger is
// models.TriggerScheduled for the stream's schedule and a gap, anything else is
// recorded as models.TriggerManual. Expected failures: ErrPITRUnavailable,
// ErrNotFound, ErrUnknownConnection, ErrUnknownStorageTarget, ErrInvalid (with
// backup.ErrEncryptionRequired while encryption is off), ErrBusy and
// ErrShuttingDown.
func (s *Service) StartBaseBackup(ctx context.Context, streamID string, trigger models.BackupTrigger) (*models.BackupRecord, error) {
	if s.cfg.PITR == nil {
		return nil, ErrPITRUnavailable
	}
	stream, err := s.cfg.PITR.GetStream(ctx, streamID)
	if err != nil {
		if errors.Is(err, pitr.ErrNotFound) {
			return nil, public("PITR stream not found", ErrNotFound, err)
		}
		return nil, fmt.Errorf("load PITR stream: %w", err)
	}
	conn, err := s.ResolveConnection(ctx, stream.ConnectionID)
	if err != nil {
		return nil, err
	}
	target, err := s.ResolveTarget(ctx, stream.TargetID)
	if err != nil {
		return nil, err
	}
	if trigger != models.TriggerScheduled {
		trigger = models.TriggerManual
	}
	// Like every backup of the connection, a base reads with the connection's
	// read preference, uploads at most at the general.max_upload_mbps cap (the
	// engine's default) and waits, shown as waiting, for a slot of the
	// connection's max_concurrent_backups.
	opts := models.BackupOptions{
		Scope: models.ScopeInstance, PITRStreamID: stream.ID, ReplicaSet: stream.ReplicaSet,
		ConnectionID: conn.ID, ConnectionName: conn.Name, MongoURI: conn.URI,
		StorageTargetID: target.ID, StorageTargetName: target.Name, StorageType: target.Type,
		Gzip: true, Trigger: trigger,
		ReadPreference: conn.ReadPref(), MaxConcurrentBackups: conn.MaxConcurrentBackups,
	}
	record, err := s.cfg.Backup.Prepare(opts)
	if err != nil {
		return nil, invalid(err)
	}

	release, err := s.cfg.Runs.Acquire(runs.PITRBaseKey(conn.ID))
	if err != nil {
		return nil, runError(err, "a base backup of this connection is already running")
	}
	snapshot := *record
	if err := s.cfg.Store.SaveBackupRecord(ctx, &snapshot); err != nil {
		release()
		return nil, fmt.Errorf("save backup record: %w", err)
	}
	tracked := s.track(models.RunBackup, record.ID, "", "")
	if err := s.cfg.Runs.Go("", func(runCtx context.Context) {
		defer release()
		defer tracked.End()
		runCtx = tracked.Bind(runCtx)
		final, runErr := s.cfg.Backup.Execute(runCtx, opts, record)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), persistTimeout)
		defer cancel()
		if saveErr := s.cfg.Store.SaveBackupRecord(persistCtx, final); saveErr != nil {
			s.logger.Error("failed to persist the record of a PITR base backup",
				logsafe.Attr("backup_id", final.ID), logsafe.Error(saveErr))
		}
		s.publish(persistCtx, events.BackupEvent(final, runErr, "", ""))
		if ve, ok := events.VerificationEvent(final, events.VerificationAfterUpload); ok {
			s.publish(persistCtx, ve)
		}
	}); err != nil {
		tracked.End()
		release()
		s.abandonBackup(ctx, &snapshot, err)
		return nil, runError(err, "")
	}
	return &snapshot, nil
}
