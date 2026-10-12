package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// PITRStreamControl pauses and resumes the collector of a PITR stream around the
// swap of an in-place point-in-time restore (implemented by *collector.Service).
type PITRStreamControl interface {
	// PauseStream disables stream id, waits for its collector to stop and ends
	// its open chain (stopped); wasEnabled reports whether it was enabled.
	PauseStream(ctx context.Context, id string) (wasEnabled bool, err error)
	// ResumeStream enables stream id and waits until its collector opened a new
	// chain.
	ResumeStream(ctx context.Context, id string) error
}

// PITRSwapper copies the clones of a completed in-place point-in-time restore
// over the original databases (implemented by *restore.Engine).
type PITRSwapper interface {
	// SnapshotPITRClones records what each clone of record holds right after the
	// restore into it; it refuses a clone that lists nothing.
	SnapshotPITRClones(ctx context.Context, req models.RestoreRequest, record *models.RestoreRecord) error
	// SwapPITRClones replaces every original database of record with its clone,
	// calling swapped with each one done.
	SwapPITRClones(ctx context.Context, req models.RestoreRequest, record *models.RestoreRecord, swapped func(db string)) error
}

// Bounds of the stream steps of an in-place point-in-time restore.
const (
	// streamPauseTimeout bounds waiting for a collector to stop.
	streamPauseTimeout = 2 * time.Minute
	// streamResumeTimeout bounds waiting for a resumed collector's new chain.
	streamResumeTimeout = 2 * time.Minute
)

// startPITRInPlace is StartRestore for an in-place point-in-time request
// (pitr.in_place): admin only (see planPITR), on the stream's own connection,
// with every replaced database confirmed (models.ErrPITRInPlaceNotConfirmed).
// With the two-person rule on it waits for a second administrator
// (models.ApprovalPITRInPlace); the approval starts the same request again. The
// run is runPITRRestore followed by finishPITRInPlace.
func (s *Service) startPITRInPlace(ctx context.Context, req models.RestoreRequest) (*models.RestoreRecord, error) {
	asked := req
	pp, err := s.planPITR(ctx, req, false)
	if err != nil {
		return nil, err
	}
	if s.cfg.PITRStreams == nil || s.cfg.PITRSwap == nil {
		return nil, public("in-place point-in-time restores are not available in this setup", ErrPITRUnavailable)
	}
	if s.needsApproval(ctx) {
		held := asked
		dbs := pp.req.PITRDatabases()
		return nil, s.requestApproval(ctx, &models.Approval{Action: models.ApprovalPITRInPlace, Subject: pp.stream.ID, Restore: &held,
			Summary: fmt.Sprintf("restore databases %s of connection %s in place to %s (point in time), replacing their current data",
				strings.Join(dbs, ", "), pp.req.TargetConnectionName, pp.run.Plan.TargetTime.Format(time.RFC3339))})
	}
	return s.runPITRRestore(ctx, pp, func(context.Context, *models.RestoreRecord) {})
}

// inPlaceLocks are the concurrency keys an in-place point-in-time restore holds:
// the backup and restore keys of every database it replaces for the whole run,
// and the base backup key of the connection until it starts the new base.
type inPlaceLocks struct {
	keys []func()
	base func()
}

// releaseBase releases the base backup key (once).
func (l *inPlaceLocks) releaseBase() {
	if l != nil && l.base != nil {
		l.base()
		l.base = nil
	}
}

// releaseAll releases every key still held (nil-safe).
func (l *inPlaceLocks) releaseAll() {
	if l == nil {
		return
	}
	l.releaseBase()
	for _, release := range l.keys {
		release()
	}
	l.keys = nil
}

// inPlaceKeys returns the database keys an in-place restore of dbs on connection
// connID holds, with what each one stands for.
func inPlaceKeys(connID string, dbs []string) (keys, what []string) {
	for _, db := range dbs {
		keys = append(keys, runs.BackupKey(connID, db), runs.RestoreKey(connID, db))
		what = append(what, "a backup of "+db, "a restore into "+db)
	}
	return append(keys, runs.PITRBaseKey(connID)), append(what, "a base backup of the connection")
}

// acquireInPlace reserves the keys of an in-place restore of dbs on connection
// connID, or fails with ErrBusy naming what runs.
func (s *Service) acquireInPlace(connID string, dbs []string) (*inPlaceLocks, error) {
	keys, what := inPlaceKeys(connID, dbs)
	l := &inPlaceLocks{}
	for i, key := range keys {
		release, err := s.cfg.Runs.Acquire(key)
		if err != nil {
			l.releaseAll()
			return nil, runError(err, what[i]+" is running; retry the in-place restore once it ended")
		}
		if key == runs.PITRBaseKey(connID) {
			l.base = release
		} else {
			l.keys = append(l.keys, release)
		}
	}
	return l, nil
}

// inPlaceStarted records that an in-place point-in-time restore was started: a
// security.destructive_action event and a log line naming who started it (and
// who approved it).
func (s *Service) inPlaceStarted(ctx context.Context, rec *models.RestoreRecord) {
	ip := rec.PITR.InPlace
	detail := fmt.Sprintf("in-place point-in-time restore %s started: databases %s of connection %s are replaced by their state at %s",
		rec.ID, strings.Join(ip.Databases, ", "), rec.TargetConnectionName, rec.PITR.TargetTime.Format(time.RFC3339))
	s.destructive(ctx, string(models.ApprovalPITRInPlace), detail, func(e *events.Event) {
		e.RestoreID, e.Databases, e.Stream, e.ConnectionID = rec.ID, ip.Databases, rec.PITR.StreamID, rec.TargetConnectionID
	})
}

// finishPITRInPlace runs the steps of an in-place point-in-time restore after
// its restore into clones (rec, runErr from ExecutePITR, which already verified
// the clones, cross-checked the oplog operations and ran the connection's
// post-restore commands on the clones):
//
//  1. a safety backup of every original database to the stream's storage
//     target, labelled models.BackupLabelPrePITRInPlace; a failure drops the
//     clones and leaves the originals untouched;
//  2. the stream is paused: its collector stops and its open chain ends;
//  3. the clones are copied over the originals (PITRSwapper); a failure keeps
//     the clones and the safety backups and names both in the message;
//  4. the clones are dropped unless pitr.keep_clone;
//  5. a stream that was enabled is enabled again with a new chain, and a base
//     backup of that chain is started.
//
// Once the clones are complete the run can no longer be cancelled (the restore
// engine marked it finishing). A failed clone restore returns unchanged: the
// originals were never touched.
func (s *Service) finishPITRInPlace(ctx context.Context, pp *pitrPlan, req models.RestoreRequest, rec *models.RestoreRecord, runErr error, locks *inPlaceLocks) (*models.RestoreRecord, error) {
	ip := rec.PITR.InPlace
	tracker := runs.FromContext(ctx)
	if runErr != nil || rec.Status != models.RestoreStatusCompleted {
		ip.Stage = models.PITRInPlaceDone
		if rec.Status == models.RestoreStatusFailed && rec.ErrorMessage != "" {
			rec.ErrorMessage += "; the original databases are untouched"
		}
		return rec, runErr
	}
	// The record is in progress again for the in-place steps.
	rec.Status, rec.CompletedAt, rec.Phases.Finished = models.RestoreStatusInProgress, nil, nil
	tlsCtx := mongotls.NewContext(ctx, req.MongoTLS)
	save := func() { s.saveInPlace(ctx, rec) }
	clonesNote := func() string { return strings.Join(rec.PITR.Clones, ", ") }
	dropClones := func() string {
		if len(rec.PITR.Clones) == 0 {
			return ""
		}
		return s.cfg.PITRRestore.DropPITRClones(tlsCtx, req.MongoURI, rec.PITR)
	}

	// 0. What the clones hold now: the swap copies exactly that, and never
	// empties an original from a clone that lists nothing.
	if err := s.cfg.PITRSwap.SnapshotPITRClones(ctx, req, rec); err != nil {
		return s.failInPlace(ctx, rec, err, fmt.Sprintf("%s; the original databases are untouched%s", redact.Text(err.Error()), dropClones()))
	}
	save()

	// 1. Safety backups.
	ip.Stage = models.PITRInPlaceSafetyBackup
	tracker.Phase(models.PhaseSafetyBackup, rec.Phases)
	save()
	for _, db := range ip.Databases {
		id, err := s.safetyBackup(ctx, pp, rec, db)
		if id != "" {
			ip.SafetyBackupIDs = append(ip.SafetyBackupIDs, id)
			save()
		}
		if err != nil {
			return s.failInPlace(ctx, rec, err, fmt.Sprintf("the safety backup of %s failed: %s; the original databases are untouched%s",
				db, redact.Text(err.Error()), dropClones()))
		}
	}

	// 2. Pause the stream. Whether it was enabled is stored before it is paused,
	// so a stop at any point from here on enables it again at the next start
	// (cleanupInterruptedInPlace).
	ip.Stage = models.PITRInPlacePausing
	ip.StreamWasEnabled = pp.stream.Enabled
	if st, getErr := s.cfg.PITR.GetStream(ctx, pp.stream.ID); getErr == nil {
		ip.StreamWasEnabled = st.Enabled
	}
	save()
	pauseCtx, cancelPause := context.WithTimeout(ctx, streamPauseTimeout)
	wasEnabled, err := s.cfg.PITRStreams.PauseStream(pauseCtx, pp.stream.ID)
	cancelPause()
	ip.StreamWasEnabled = ip.StreamWasEnabled || wasEnabled
	if err != nil {
		note := s.resumeInPlace(ctx, pp, rec, locks, false)
		return s.failInPlace(ctx, rec, err, fmt.Sprintf("the PITR stream could not be paused: %s; the original databases are untouched, the safety backup(s) %s are kept%s%s",
			redact.Text(err.Error()), strings.Join(ip.SafetyBackupIDs, ", "), dropClones(), note))
	}
	ip.StreamPaused = true
	save()
	tracker.Printf("PITR stream %s paused (it was %s); its chain ended", pp.stream.ID, map[bool]string{true: "enabled", false: "disabled"}[wasEnabled])

	// 3. The swap.
	ip.Stage, ip.SwapStarted = models.PITRInPlaceSwapping, true
	tracker.Phase(models.PhaseSwapping, rec.Phases)
	save()
	err = s.cfg.PITRSwap.SwapPITRClones(ctx, req, rec, func(db string) {
		ip.Swapped = append(ip.Swapped, db)
		save()
	})
	ip.Stage = models.PITRInPlaceResuming
	if err != nil {
		note := s.resumeInPlace(ctx, pp, rec, locks, true)
		return s.failInPlace(ctx, rec, err, swapFailedMessage(ip, err, clonesNote())+note)
	}
	rec.Phases.RestoreDone = models.Stamp(time.Now())

	// 4. The clones.
	if ip.KeepClone {
		addInPlaceWarning(rec, "the clones "+clonesNote()+" were kept (keep_clone); drop them when done")
	} else if note := dropClones(); strings.Contains(note, "manually") {
		addInPlaceWarning(rec, strings.TrimPrefix(note, "; "))
	}

	// 5. The stream.
	if note := s.resumeInPlace(ctx, pp, rec, locks, true); note != "" {
		addInPlaceWarning(rec, strings.TrimPrefix(note, "; "))
	}
	// The restored state now lives in the originals.
	rec.TargetDatabase = strings.Join(ip.Swapped, ",")
	finish := time.Now().UTC()
	rec.Status, rec.CompletedAt = models.RestoreStatusCompleted, &finish
	rec.DurationSeconds = finish.Sub(rec.StartedAt).Seconds()
	rec.Phases.Finished = models.Stamp(finish)
	ip.Stage = models.PITRInPlaceDone
	tracker.Phase(models.PhaseFinishing, rec.Phases)
	tracker.Printf("in-place point-in-time restore completed: %s hold their state at %s (safety backup(s) %s)",
		strings.Join(ip.Swapped, ", "), rec.PITR.TargetTime.Format(time.RFC3339), strings.Join(ip.SafetyBackupIDs, ", "))
	s.logger.Info("in-place point-in-time restore completed", logsafe.Attr("restore_id", rec.ID),
		logsafe.Attr("databases", strings.Join(ip.Swapped, ",")), slog.Float64("duration_sec", rec.DurationSeconds))
	return rec, nil
}

// swapFailedMessage explains a failed swap and how to recover from it.
func swapFailedMessage(ip *models.PITRInPlace, err error, clones string) string {
	var untouched []string
	for _, db := range ip.Databases {
		if !containsString(ip.Swapped, db) {
			untouched = append(untouched, db)
		}
	}
	msg := fmt.Sprintf("replacing the original databases failed: %s.", redact.Text(err.Error()))
	if len(ip.Swapped) > 0 {
		msg += fmt.Sprintf(" %s already hold the restored state.", strings.Join(ip.Swapped, ", "))
	}
	if len(untouched) > 0 {
		msg += fmt.Sprintf(" %s may be partially replaced (the first) or untouched.", strings.Join(untouched, ", "))
	}
	return msg + fmt.Sprintf(" The restored state is kept in the clone(s) %s and the state before the swap in the safety backup(s) %s: "+
		"to go back, restore each safety backup in place with drop_target; to finish, copy each clone over its original "+
		"(mongodump --archive --db=<clone> | mongorestore --archive --nsFrom='<clone>.*' --nsTo='<db>.*' --drop) and drop the clones",
		clones, strings.Join(ip.SafetyBackupIDs, ", "))
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// addInPlaceWarning appends w to the record's warnings.
func addInPlaceWarning(rec *models.RestoreRecord, w string) {
	if rec.Warning != "" {
		rec.Warning += "; "
	}
	rec.Warning += w
}

// failInPlace marks rec failed with message after the clone restore completed.
func (s *Service) failInPlace(ctx context.Context, rec *models.RestoreRecord, err error, message string) (*models.RestoreRecord, error) {
	finish := time.Now().UTC()
	rec.Status, rec.CompletedAt = models.RestoreStatusFailed, &finish
	rec.DurationSeconds = finish.Sub(rec.StartedAt).Seconds()
	rec.Phases.Finished = models.Stamp(finish)
	rec.ErrorMessage = redact.Text(message)
	rec.PITR.InPlace.Stage = models.PITRInPlaceDone
	tracker := runs.FromContext(ctx)
	tracker.Phase(models.PhaseFinishing, rec.Phases)
	tracker.Printf("in-place point-in-time restore failed: %s", rec.ErrorMessage)
	s.logger.Error("in-place point-in-time restore failed", logsafe.Attr("restore_id", rec.ID), logsafe.Error(err))
	return rec, fmt.Errorf("in-place point-in-time restore: %w", err)
}

// saveInPlace stores the progress of an in-place restore, so a restart knows how
// far it got (see CleanupInterruptedPITR).
func (s *Service) saveInPlace(ctx context.Context, rec *models.RestoreRecord) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	cp := *rec
	info := *rec.PITR
	ip := *rec.PITR.InPlace
	info.InPlace = &ip
	cp.PITR = &info
	if err := s.cfg.Store.SaveRestoreRecord(saveCtx, &cp); err != nil {
		s.cfg.DiskGuard.Observe(saveCtx, err)
		s.logger.Error("failed to record the progress of an in-place point-in-time restore",
			logsafe.Attr("restore_id", rec.ID), logsafe.Error(err))
	}
}

// safetyBackup takes the safety backup of original database db of rec: a full
// backup to the stream's storage target, read from the primary, labelled
// models.BackupLabelPrePITRInPlace and linked to rec. It runs in this run
// (the database's backup key is held by the in-place restore), tracked as a
// backup run of its own, and is stored and announced like a manual backup. It
// returns the backup's ID (empty when none was recorded).
func (s *Service) safetyBackup(ctx context.Context, pp *pitrPlan, rec *models.RestoreRecord, db string) (string, error) {
	tracker := runs.FromContext(ctx)
	target, err := s.ResolveTarget(ctx, pp.stream.TargetID)
	if err != nil {
		return "", fmt.Errorf("storage target of the stream: %w", err)
	}
	req := pp.req
	opts := models.BackupOptions{
		Database: db, ConnectionID: req.TargetConnectionID, ConnectionName: req.TargetConnectionName,
		MongoURI: req.MongoURI, MongoTLS: req.MongoTLS,
		StorageTargetID: target.ID, StorageTargetName: target.Name, StorageType: target.Type,
		Gzip: true, Trigger: models.TriggerManual, ReadPreference: models.ReadPreference{Mode: "primary"},
	}
	record, err := s.cfg.Backup.Prepare(opts)
	if err != nil {
		return "", err
	}
	record.Label, record.RestoreID = models.BackupLabelPrePITRInPlace, rec.ID
	if err := s.store.SaveBackupRecord(ctx, record); err != nil {
		return "", s.startSaveError(ctx, "backup", err)
	}
	tracker.Printf("safety backup %s of %s to storage target %s started", record.ID, db, target.Name)
	backupRun := s.track(models.RunBackup, record.ID, "", db)
	final, runErr := s.cfg.Backup.Execute(backupRun.Bind(ctx), opts, record)
	backupRun.End()
	if final == nil {
		final = record
	}
	final.Label, final.RestoreID = models.BackupLabelPrePITRInPlace, rec.ID
	s.finishBackup(ctx, final, runErr, "", db)
	if runErr == nil && final.Status != models.StatusCompleted {
		runErr = fmt.Errorf("backup %s ended %s: %s", final.ID, final.Status, final.ErrorMessage)
	}
	if runErr != nil {
		tracker.Printf("safety backup %s of %s failed", final.ID, db)
		return final.ID, runErr
	}
	tracker.Printf("safety backup %s of %s completed (%d bytes)", final.ID, db, final.SizeBytes)
	return final.ID, nil
}

// resumeInPlace enables the stream of an in-place restore again when the restore
// paused it while it was enabled, waits for its new chain and, when withBase,
// starts the base backup of that chain. It returns a note for the record (empty
// when everything went well).
func (s *Service) resumeInPlace(ctx context.Context, pp *pitrPlan, rec *models.RestoreRecord, locks *inPlaceLocks, withBase bool) string {
	ip := rec.PITR.InPlace
	locks.releaseBase()
	if !ip.StreamWasEnabled {
		return ""
	}
	tracker := runs.FromContext(ctx)
	resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamResumeTimeout)
	defer cancel()
	if err := s.cfg.PITRStreams.ResumeStream(resumeCtx, pp.stream.ID); err != nil {
		s.logger.Warn("in-place point-in-time restore: the PITR stream could not be resumed",
			logsafe.Attr("restore_id", rec.ID), logsafe.Attr("stream_id", pp.stream.ID), logsafe.Error(err))
		return fmt.Sprintf("; the PITR stream %s could not be enabled again (%s): enable it and take a base backup", pp.stream.ID, redact.Text(err.Error()))
	}
	ip.StreamResumed = true
	tracker.Printf("PITR stream %s enabled again with a new chain", pp.stream.ID)
	if !withBase {
		return ""
	}
	base, err := s.StartBaseBackup(context.WithoutCancel(ctx), pp.stream.ID, models.TriggerManual)
	if err != nil {
		if errors.Is(err, runs.ErrBusy) {
			return ""
		}
		return fmt.Sprintf("; the base backup of the stream's new chain could not be started (%s): take one", redact.Text(err.Error()))
	}
	ip.NewBaseID = base.ID
	tracker.Printf("base backup %s of the new chain started", base.ID)
	return ""
}

// cleanupInterruptedInPlace settles rec, an in-place point-in-time restore that a
// stop interrupted after its clone restore (see CleanupInterruptedPITR), and
// returns a note for its message. Before the swap the originals are untouched and
// the clones are dropped; once the swap started the clones are kept, because they
// may hold the only copy of the restored state, and the note names them and the
// safety backups. A stream the restore paused is enabled again.
func (s *Service) cleanupInterruptedInPlace(ctx context.Context, rec *models.RestoreRecord) string {
	ip := rec.PITR.InPlace
	var note string
	switch {
	case ip.SwapStarted && len(ip.Swapped) == len(ip.Databases) && ip.Stage == models.PITRInPlaceDone:
		note = "; the original databases hold the restored state"
	case ip.SwapStarted:
		note = "; " + swapFailedMessage(ip, errors.New("MongoRescue stopped during the swap"), strings.Join(rec.PITR.Clones, ", "))
	default:
		note = "; the original databases are untouched"
		if len(ip.SafetyBackupIDs) > 0 {
			note += ", the safety backup(s) " + strings.Join(ip.SafetyBackupIDs, ", ") + " are kept"
		}
		if conn, err := s.ResolveConnection(ctx, rec.TargetConnectionID); err == nil {
			note += s.cfg.PITRRestore.DropPITRClones(mongotls.NewContext(ctx, conn.TLS()), conn.URI, rec.PITR)
		} else if len(rec.PITR.Clones) > 0 {
			note += fmt.Sprintf("; the clones %s could not be dropped (%s), drop them manually", strings.Join(rec.PITR.Clones, ", "), redact.Text(err.Error()))
		}
	}
	// The stream is enabled again whenever the restore got as far as pausing it
	// (its state is stored before the pause), whatever the stage it stopped in.
	if ip.Stage != models.PITRInPlaceRestoring && ip.StreamWasEnabled && !ip.StreamResumed {
		note += s.resumeInterrupted(ctx, rec)
	}
	return note
}

// resumeInterrupted enables the stream an interrupted in-place restore paused
// and, in the background, waits for its collector's new chain and starts the base
// backup of that chain. A stream that cannot be enabled raises
// pitr.collector_failed. It returns a note for the record.
func (s *Service) resumeInterrupted(ctx context.Context, rec *models.RestoreRecord) string {
	id := rec.PITR.StreamID
	if s.cfg.PITRStreams == nil {
		s.alertResume(ctx, rec, errors.New("PITR streams cannot be controlled in this setup"))
		return "; the PITR stream " + id + " was paused by this restore: enable it and take a base backup"
	}
	// The collector may not run yet at startup: the stream is enabled now, and
	// its new chain opens once the collector runs.
	enableCtx, cancel := context.WithTimeout(ctx, time.Second)
	err := s.cfg.PITRStreams.ResumeStream(enableCtx, id)
	cancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		s.alertResume(ctx, rec, err)
		return fmt.Sprintf("; the PITR stream %s could not be enabled again (%s): enable it and take a base backup", id, redact.Text(err.Error()))
	}
	goErr := s.cfg.Runs.Go("", func(runCtx context.Context) {
		waitCtx, cancelWait := context.WithTimeout(runCtx, streamResumeTimeout)
		defer cancelWait()
		if waitErr := s.cfg.PITRStreams.ResumeStream(waitCtx, id); waitErr != nil {
			s.alertResume(runCtx, rec, waitErr)
			return
		}
		base, baseErr := s.StartBaseBackup(runCtx, id, models.TriggerManual)
		if baseErr != nil {
			s.logger.Warn("the base backup of a PITR stream resumed after an interrupted in-place restore could not be started; the base schedule retries it",
				logsafe.Attr("stream_id", id), logsafe.Attr("restore_id", rec.ID), logsafe.Error(baseErr))
			return
		}
		s.logger.Info("PITR stream resumed after an interrupted in-place restore", logsafe.Attr("stream_id", id),
			logsafe.Attr("restore_id", rec.ID), logsafe.Attr("backup_id", base.ID))
	})
	if goErr != nil {
		return "; the PITR stream " + id + " was enabled again: take a base backup of its new chain"
	}
	return "; the PITR stream " + id + " was enabled again, and a base backup of its new chain is started once its collector runs"
}

// alertResume raises pitr.collector_failed for the stream an interrupted in-place
// restore paused and that could not be enabled again: until someone does, no
// oplog is collected.
func (s *Service) alertResume(ctx context.Context, rec *models.RestoreRecord, err error) {
	msg := redact.Text(err.Error())
	s.logger.Error("the PITR stream paused by an interrupted in-place restore could not be enabled again",
		logsafe.Attr("stream_id", rec.PITR.StreamID), logsafe.Attr("restore_id", rec.ID), logsafe.Attr("error", msg))
	s.publish(context.WithoutCancel(ctx), events.Event{Type: events.PITRCollectorFailed, Time: s.now().UTC(), Status: "failed",
		Error: msg, Stream: rec.PITR.StreamID, ConnectionID: rec.TargetConnectionID, RestoreID: rec.ID,
		Detail: "the in-place point-in-time restore " + rec.ID + " paused this stream and MongoRescue stopped before it was enabled again; enable it and take a base backup"})
}

// pitrInPlace reports what an in-place point-in-time restore does to the
// databases it replaces and fails while a backup or restore holds one of them.
func (p *preflightRun) pitrInPlace(pp *pitrPlan, ip *models.PITRInPlace) {
	const id = models.PreflightCheckInPlace
	if p.svc.cfg.PITRStreams == nil || p.svc.cfg.PITRSwap == nil {
		p.res.Add(id, models.PreflightFail, "in-place point-in-time restores are not available in this setup")
		return
	}
	keys, what := inPlaceKeys(pp.req.TargetConnectionID, ip.Databases)
	for i, key := range keys {
		release, err := p.svc.cfg.Runs.Acquire(key)
		if err != nil {
			if errors.Is(err, runs.ErrBusy) {
				p.res.Add(id, models.PreflightFail, what[i]+" is running; retry once it ended")
				return
			}
			p.res.Add(id, models.PreflightWarn, "could not check for running backups and restores: "+errText(err))
			return
		}
		release()
	}
	target := pp.stream.TargetID
	if t, err := p.svc.ResolveTarget(p.ctx, target); err == nil {
		target = t.Name
	}
	p.res.Add(id, models.PreflightPass, fmt.Sprintf(
		"replaces the databases %s: after the restore into clones, a safety backup of each goes to storage target %s, "+
			"the PITR stream is paused and the clones are copied over the originals; applications must stop writing to them until it completes",
		strings.Join(ip.Databases, ", "), target))
}
