package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// In-place point-in-time restores in active-passive HA (docs/design/ha.md, "In-place
// PITR"). The swap outlasts the lease: the instance that runs it may be fenced
// off, or die, while mongorestore still writes the original databases. The
// instance that takes over therefore never treats such a swap as interrupted by
// itself. It finds the swap's marker on the HA target (models.SwapLedger), which
// the swapping instance wrote under its own epoch before it replaced anything,
// and leaves the swap, its clones, its databases and its stream alone until the
// swapping process reports that it stopped, or a bounded time passed. It never
// resumes a swap: once settled, the record says what is left and where.

const (
	// DefaultSwapHandoverTimeout is how long an instance that took over leaves a
	// swap of an older epoch alone when its process says nothing: the longest a
	// swap can run (MaxSwapDuration, enforced by the process that swaps) and a
	// margin.
	DefaultSwapHandoverTimeout = MaxSwapDuration + 10*time.Minute
	// DefaultSwapHandoverPoll is how often a held swap's marker is read.
	DefaultSwapHandoverPoll = 10 * time.Second
	// swapLedgerTimeout bounds one write of the swap ledger.
	swapLedgerTimeout = 30 * time.Second
)

// Outcomes of a handed-over swap, as counted by SwapHandoverCounts: the labels of
// mongorescue_ha_pitr_swap_handovers_total.
const (
	// SwapHandoverCompleted: the swapping process reported a completed swap.
	SwapHandoverCompleted = "completed"
	// SwapHandoverFailed: it reported a failed or aborted swap.
	SwapHandoverFailed = "failed"
	// SwapHandoverTimeout: it reported nothing within the timeout (or its marker
	// was removed); the swap was marked interrupted.
	SwapHandoverTimeout = "timeout"
)

// heldSwap is a swap of an older epoch this instance leaves alone.
type heldSwap struct {
	marker models.SwapMarker
	// since is when this instance first saw the marker; its monotonic reading
	// times the wait.
	since time.Time
	// release frees the concurrency keys held for the swap's databases.
	release func()
}

// heldSwaps is the registry of the swaps this instance leaves alone.
type heldSwaps struct {
	mu       sync.Mutex
	held     map[string]*heldSwap
	outcomes map[string]uint64
	// monitoring reports that the monitor goroutine runs.
	monitoring bool
}

// HeldSwaps returns how many swaps of an older epoch this instance leaves alone.
func (s *Service) HeldSwaps() int {
	s.swaps.mu.Lock()
	defer s.swaps.mu.Unlock()
	return len(s.swaps.held)
}

// SwapHandoverCounts returns how many swaps of an older epoch this instance
// settled, by outcome (every outcome present, 0 included).
func (s *Service) SwapHandoverCounts() map[string]uint64 {
	s.swaps.mu.Lock()
	defer s.swaps.mu.Unlock()
	out := map[string]uint64{SwapHandoverCompleted: 0, SwapHandoverFailed: 0, SwapHandoverTimeout: 0}
	for k, v := range s.swaps.outcomes {
		out[k] = v
	}
	return out
}

func (s *Service) swapHandoverTimeout() time.Duration {
	if s.cfg.SwapHandoverTimeout > 0 {
		return s.cfg.SwapHandoverTimeout
	}
	return DefaultSwapHandoverTimeout
}

func (s *Service) swapHandoverPoll() time.Duration {
	if s.cfg.SwapHandoverPoll > 0 {
		return s.cfg.SwapHandoverPoll
	}
	return DefaultSwapHandoverPoll
}

// swapMarker returns the ledger marker of the swap of rec, about to start.
func (s *Service) swapMarker(rec *models.RestoreRecord) models.SwapMarker {
	ip := rec.PITR.InPlace
	m := models.SwapMarker{
		RestoreID: rec.ID, StreamID: rec.PITR.StreamID, ConnectionID: rec.TargetConnectionID,
		Databases: slices.Clone(ip.Databases), Clones: slices.Clone(rec.PITR.Clones),
		StreamWasEnabled: ip.StreamWasEnabled, KeepClone: ip.KeepClone, SafetyBackupIDs: slices.Clone(ip.SafetyBackupIDs),
		StartedAt: s.now().UTC(),
	}
	if s.cfg.Fence != nil {
		m.Epoch = s.cfg.Fence.Epoch()
	}
	return m
}

// fenced reports that this instance runs with a swap ledger and no longer holds
// the lease.
func (s *Service) fenced() bool {
	return s.cfg.SwapLedger != nil && s.checkFence() != nil
}

// finishSwap tells the HA target that the swap of rec stopped writing, with its
// outcome. A failure is logged and noted on the record: the instance that took
// over then waits for its timeout instead.
func (s *Service) finishSwap(ctx context.Context, rec *models.RestoreRecord, outcome models.SwapOutcome) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swapLedgerTimeout)
	defer cancel()
	if err := s.cfg.SwapLedger.Finish(finishCtx, rec.ID, outcome, rec.PITR.InPlace.Swapped); err != nil {
		s.logger.ErrorContext(ctx, "could not record the end of an in-place point-in-time swap on the HA target",
			logsafe.Attr("restore_id", rec.ID), logsafe.Error(err))
		addInPlaceWarning(rec, "the end of the swap could not be recorded on the HA target ("+redact.Text(err.Error())+
			"): an instance that takes over waits for its handover timeout before it settles this restore")
	}
}

// resolveSwap removes the marker of rec's swap once this instance settled the
// restore itself. A fenced instance leaves it for the instance that holds the
// lease, which settles the restore from it.
func (s *Service) resolveSwap(ctx context.Context, rec *models.RestoreRecord) {
	if s.fenced() {
		return
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swapLedgerTimeout)
	defer cancel()
	if err := s.cfg.SwapLedger.Resolve(resolveCtx, rec.ID); err != nil {
		s.logger.WarnContext(ctx, "could not remove the swap marker of a finished in-place point-in-time restore from the HA target",
			logsafe.Attr("restore_id", rec.ID), logsafe.Error(err))
	}
}

// resumeAfterSwap enables the paused stream again, as resumeInPlace does, unless
// this instance lost the HA lease meanwhile: then the instance that holds it
// does, when it settles the restore from the swap marker (a fenced instance
// changes nothing but what it started).
func (s *Service) resumeAfterSwap(ctx context.Context, pp *pitrPlan, rec *models.RestoreRecord, locks *inPlaceLocks, withBase, ledger bool) string {
	if ledger && s.fenced() {
		locks.releaseBase()
		return fmt.Sprintf("; this instance lost the HA lease: the instance that holds it enables the PITR stream %s again and takes a base backup of its new chain", pp.stream.ID)
	}
	return s.resumeInPlace(ctx, pp, rec, locks, withBase)
}

// AdoptSwaps is called by an instance that just took over, before the startup
// sweep and before any component starts. It lists the swap markers on the HA
// target and, for each one:
//
//   - a marker its process finished is settled now (see settleSwap);
//   - a marker still swapping is held: the keys of its databases and connection
//     stay reserved (no backup, restore or base backup touches them), its stream
//     is paused (disabled, its chain ended) so the collector does not start, and
//     the restore's record is left alone. A monitor settles it once the process
//     reports its outcome, or Config.SwapHandoverTimeout passed on this
//     instance's own clock, when it marks the restore interrupted and alerts.
//
// It returns the IDs of the restores the startup sweep must leave alone. It
// fails, so the takeover is abandoned and retried, when the ledger cannot be read
// or a held swap's stream cannot be paused: sweeping blindly could re-enable the
// stream while the swap still writes. It does nothing without Config.SwapLedger.
func (s *Service) AdoptSwaps(ctx context.Context) (map[string]bool, error) {
	ledger := s.cfg.SwapLedger
	if ledger == nil {
		return nil, nil
	}
	markers, err := ledger.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("in-place PITR swaps: %w", err)
	}
	adopted := make(map[string]bool, len(markers))
	for _, m := range markers {
		adopted[m.RestoreID] = true
		if m.Finished() {
			s.settleSwap(ctx, m, settleFinished)
			continue
		}
		if err = s.holdSwap(ctx, m); err != nil {
			s.releaseHeld()
			return nil, err
		}
	}
	if s.HeldSwaps() > 0 {
		s.startSwapMonitor()
	}
	return adopted, nil
}

// holdSwap leaves the swap of m alone: it reserves its keys and pauses its
// stream.
func (s *Service) holdSwap(ctx context.Context, m models.SwapMarker) error {
	var releases []func()
	keys, _ := inPlaceKeys(m.ConnectionID, m.Databases)
	for _, key := range keys {
		release, err := s.cfg.Runs.Acquire(key)
		if err != nil {
			s.logger.WarnContext(ctx, "a key of an in-place point-in-time swap of an older epoch is already in use",
				logsafe.Attr("restore_id", m.RestoreID), logsafe.Attr("key", key), logsafe.Error(err))
			continue
		}
		releases = append(releases, release)
	}
	freeAll := func() {
		for _, r := range releases {
			r()
		}
	}
	if m.StreamID != "" && s.cfg.PITRStreams != nil {
		pauseCtx, cancel := context.WithTimeout(ctx, streamPauseTimeout)
		_, err := s.cfg.PITRStreams.PauseStream(pauseCtx, m.StreamID)
		cancel()
		if err != nil && !isStreamGone(err) {
			freeAll()
			return fmt.Errorf("pause the PITR stream %s of the in-place swap %s: %w", m.StreamID, m.RestoreID, err)
		}
	}
	s.swaps.mu.Lock()
	if s.swaps.held == nil {
		s.swaps.held = map[string]*heldSwap{}
	}
	s.swaps.held[m.RestoreID] = &heldSwap{marker: m, since: s.now(), release: freeAll}
	s.swaps.mu.Unlock()
	s.logger.WarnContext(ctx, "an in-place point-in-time swap of an older HA epoch may still be writing: this instance leaves it, its databases and its stream alone until it reports its end or the handover timeout passes",
		logsafe.Attr("restore_id", m.RestoreID), logsafe.Attr("databases", strings.Join(m.Databases, ",")),
		slog.Uint64("swap_epoch", m.Epoch), slog.Duration("timeout", s.swapHandoverTimeout()))
	return nil
}

// isStreamGone reports an error of a stream that does not exist (any more).
func isStreamGone(err error) bool { return errors.Is(err, pitr.ErrNotFound) }

// releaseHeld frees every held swap's keys and forgets them.
func (s *Service) releaseHeld() {
	s.swaps.mu.Lock()
	defer s.swaps.mu.Unlock()
	for id, h := range s.swaps.held {
		h.release()
		delete(s.swaps.held, id)
	}
}

// startSwapMonitor runs the goroutine that settles held swaps, bound to the run
// manager's lifecycle.
func (s *Service) startSwapMonitor() {
	s.swaps.mu.Lock()
	if s.swaps.monitoring {
		s.swaps.mu.Unlock()
		return
	}
	s.swaps.monitoring = true
	s.swaps.mu.Unlock()
	err := s.cfg.Runs.Go("", func(ctx context.Context) {
		defer func() {
			s.swaps.mu.Lock()
			s.swaps.monitoring = false
			s.swaps.mu.Unlock()
		}()
		s.monitorSwaps(ctx)
	})
	if err != nil {
		s.swaps.mu.Lock()
		s.swaps.monitoring = false
		s.swaps.mu.Unlock()
		s.logger.Error("could not start the monitor of the in-place point-in-time swaps of an older HA epoch", logsafe.Error(err))
	}
}

// monitorSwaps settles the held swaps until none is left or ctx ends.
func (s *Service) monitorSwaps(ctx context.Context) {
	ticker := time.NewTicker(s.swapHandoverPoll())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.HeldSwaps() == 0 {
			return
		}
		s.pollSwaps(ctx)
	}
}

// pollSwaps reads the markers once and settles the held swaps whose process
// reported its end, whose marker is gone, or whose wait is over. An instance that
// lost the lease settles nothing.
func (s *Service) pollSwaps(ctx context.Context) {
	if err := s.checkFence(); err != nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, swapLedgerTimeout)
	markers, err := s.cfg.SwapLedger.Open(readCtx)
	cancel()
	if err != nil {
		s.logger.WarnContext(ctx, "could not read the swap ledger; held in-place point-in-time swaps stay held", logsafe.Error(err))
		return
	}
	current := make(map[string]models.SwapMarker, len(markers))
	for _, m := range markers {
		current[m.RestoreID] = m
	}
	s.swaps.mu.Lock()
	held := make([]*heldSwap, 0, len(s.swaps.held))
	for _, h := range s.swaps.held {
		held = append(held, h)
	}
	s.swaps.mu.Unlock()
	timeout := s.swapHandoverTimeout()
	for _, h := range held {
		m, ok := current[h.marker.RestoreID]
		switch {
		case ok && m.Finished():
			s.settleSwap(ctx, m, settleFinished)
		case !ok:
			s.settleSwap(ctx, h.marker, settleRemoved)
		case s.now().Sub(h.since) >= timeout:
			s.settleSwap(ctx, m, settleTimeout)
		}
	}
}

// How a swap of an older epoch was settled.
type settleHow int

const (
	// settleFinished: its process reported its outcome.
	settleFinished settleHow = iota
	// settleTimeout: its process said nothing within the handover timeout.
	settleTimeout
	// settleRemoved: its marker was removed from the HA target by hand.
	settleRemoved
)

// settleSwap settles the restore of m, a swap of an older epoch this instance
// left alone, and removes its marker. It never touches the original databases:
//
//   - the record (from the metadata snapshot, possibly older than the swap) ends
//     completed when the process reported a completed swap and failed otherwise,
//     with a message that names the clones and safety backups left and how to go
//     on; a timeout says that a process of the swap may still write;
//   - an aborted swap's clones are dropped, since the originals are untouched;
//   - the stream the restore paused is enabled again, which opens a new chain, and
//     a base backup of it starts, never before.
func (s *Service) settleSwap(ctx context.Context, m models.SwapMarker, how settleHow) {
	rec, err := s.cfg.Store.GetRestoreRecord(ctx, m.RestoreID)
	missing := errors.Is(err, store.ErrNotFound)
	if err != nil && !missing {
		s.logger.WarnContext(ctx, "could not read the record of an in-place point-in-time swap to settle; it is retried",
			logsafe.Attr("restore_id", m.RestoreID), logsafe.Error(err))
		return
	}
	if missing {
		// The metadata snapshot this instance took over from predates the restore:
		// there is no record to update, and the event and the log carry the outcome.
		rec = markerRecord(m)
	}
	if rec.PITR == nil || rec.PITR.InPlace == nil {
		// A record of another kind under this ID: nothing of ours to settle.
		s.logger.ErrorContext(ctx, "a swap marker names a restore that is not an in-place point-in-time restore; it is left on the HA target",
			logsafe.Attr("restore_id", m.RestoreID))
		s.forgetHeld(m.RestoreID)
		return
	}
	ip := rec.PITR.InPlace
	wasOpen := rec.Status == models.RestoreStatusInProgress || rec.Status == models.RestoreStatusPending
	ip.StreamWasEnabled = ip.StreamWasEnabled || m.StreamWasEnabled
	if len(ip.SafetyBackupIDs) == 0 {
		ip.SafetyBackupIDs = slices.Clone(m.SafetyBackupIDs)
	}
	if len(rec.PITR.Clones) == 0 {
		rec.PITR.Clones = slices.Clone(m.Clones)
	}
	outcome := SwapHandoverFailed
	var note string
	var failure error
	if wasOpen {
		ip.SwapStarted = m.Outcome != models.SwapAborted
		if m.Finished() {
			ip.Swapped = slices.Clone(m.Swapped)
		}
		switch {
		case how == settleFinished && m.Outcome == models.SwapCompleted:
			outcome = SwapHandoverCompleted
			note = "; the original databases hold the restored state"
			ip.Swapped = slices.Clone(ip.Databases)
		case how == settleFinished && m.Outcome == models.SwapAborted:
			failure = errors.New("the instance that started this restore lost the HA lease before it replaced a database")
			note = "; " + failure.Error() + "; the original databases are untouched"
			if len(ip.SafetyBackupIDs) > 0 {
				note += ", the safety backup(s) " + strings.Join(ip.SafetyBackupIDs, ", ") + " are kept"
			}
			note += s.dropSettledClones(ctx, rec)
		case how == settleFinished:
			failure = errors.New("the swap stopped on the instance that ran it")
			note = "; " + swapFailedMessage(rec, failure)
		default:
			outcome = SwapHandoverTimeout
			reason := fmt.Sprintf("the instance that ran the swap (HA epoch %d) reported nothing within %s", m.Epoch, s.swapHandoverTimeout())
			if how == settleRemoved {
				reason = "the swap's marker was removed from the HA target"
			}
			failure = errors.New("interrupted: " + reason)
			note = "; " + swapFailedMessage(rec, failure) +
				" A process of the swap may still be writing to the original databases: check them before you rely on them, then verify them against the safety backup(s)."
		}
	}
	// The stream the restore paused is enabled again, whatever became of the swap:
	// its collector opens a new chain, and the base backup of that chain starts.
	if ip.StreamWasEnabled && !ip.StreamResumed {
		// The metadata snapshot may predate the pause: end the open chain (a no-op
		// for one that ended) so the swap's writes never join it.
		if m.StreamID != "" && s.cfg.PITRStreams != nil {
			pauseCtx, cancelPause := context.WithTimeout(ctx, streamPauseTimeout)
			if _, pauseErr := s.cfg.PITRStreams.PauseStream(pauseCtx, m.StreamID); pauseErr != nil && !isStreamGone(pauseErr) {
				s.logger.WarnContext(ctx, "could not pause the PITR stream of a settled in-place swap before enabling it again",
					logsafe.Attr("restore_id", m.RestoreID), logsafe.Error(pauseErr))
			}
			cancelPause()
		}
		resumeNote := s.resumeInterrupted(ctx, rec)
		ip.StreamResumed = !strings.Contains(resumeNote, "could not")
		if wasOpen {
			note += resumeNote
		}
	}
	if wasOpen {
		now := s.now().UTC()
		ip.Stage = models.PITRInPlaceDone
		rec.CompletedAt, rec.Phases.Finished = &now, models.Stamp(now)
		if outcome == SwapHandoverCompleted {
			rec.Status, rec.TargetDatabase = models.RestoreStatusCompleted, strings.Join(ip.Swapped, ",")
			addInPlaceWarning(rec, "finished on the instance that ran it and settled by the instance that took over"+note)
		} else {
			rec.Status = models.RestoreStatusFailed
			rec.ErrorMessage = redact.Text("interrupted: the HA instance running this restore was replaced" + note)
		}
		if !missing {
			if saveErr := s.cfg.Store.SaveRestoreRecord(ctx, rec); saveErr != nil {
				s.logger.ErrorContext(ctx, "could not save the settled record of an in-place point-in-time swap",
					logsafe.Attr("restore_id", rec.ID), logsafe.Error(saveErr))
			}
		}
		s.announceSettled(ctx, rec, failure, outcome)
	}
	s.swaps.mu.Lock()
	if s.swaps.outcomes == nil {
		s.swaps.outcomes = map[string]uint64{}
	}
	s.swaps.outcomes[outcome]++
	s.swaps.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swapLedgerTimeout)
	if resErr := s.cfg.SwapLedger.Resolve(resolveCtx, m.RestoreID); resErr != nil {
		s.logger.WarnContext(ctx, "could not remove a settled swap marker from the HA target",
			logsafe.Attr("restore_id", m.RestoreID), logsafe.Error(resErr))
	}
	cancel()
	s.forgetHeld(m.RestoreID)
	s.logger.WarnContext(ctx, "in-place point-in-time swap of an older HA epoch settled", logsafe.Attr("restore_id", m.RestoreID),
		slog.String("outcome", outcome), slog.Uint64("swap_epoch", m.Epoch))
}

// announceSettled publishes the events of a restore this instance settled: the
// usual restore and in-place events, and for an interruption an alert queued
// before it returns.
func (s *Service) announceSettled(ctx context.Context, rec *models.RestoreRecord, failure error, outcome string) {
	pubCtx := context.WithoutCancel(ctx)
	s.publish(pubCtx, events.RestoreEvent(rec, failure, rec.BackupID))
	e := events.InPlacePITREvent(rec, failure)
	if outcome == SwapHandoverTimeout {
		e.Detail = strings.TrimSpace(e.Detail + " the instance that ran the swap reported nothing; the original databases may be partially replaced")
		s.alert(pubCtx, e)
		return
	}
	s.publish(pubCtx, e)
}

// dropSettledClones drops the clones recorded on rec (an aborted swap's) and
// returns a note.
func (s *Service) dropSettledClones(ctx context.Context, rec *models.RestoreRecord) string {
	if s.cfg.PITRRestore == nil || len(rec.PITR.Clones) == 0 {
		return ""
	}
	conn, err := s.ResolveConnection(ctx, rec.TargetConnectionID)
	if err != nil {
		return fmt.Sprintf("; the clones %s could not be dropped (%s), drop them manually", strings.Join(rec.PITR.Clones, ", "), redact.Text(err.Error()))
	}
	return s.cfg.PITRRestore.DropPITRClones(mongotls.NewContext(ctx, conn.TLS()), conn.URI, rec.PITR)
}

// forgetHeld frees the keys of the held swap of restoreID and forgets it.
func (s *Service) forgetHeld(restoreID string) {
	s.swaps.mu.Lock()
	h := s.swaps.held[restoreID]
	delete(s.swaps.held, restoreID)
	s.swaps.mu.Unlock()
	if h != nil {
		h.release()
	}
}

// markerRecord builds the in-memory record of a swap whose restore record the
// metadata snapshot did not hold, from its marker.
func markerRecord(m models.SwapMarker) *models.RestoreRecord {
	suffix := ""
	if len(m.Clones) > 0 && len(m.Databases) > 0 {
		suffix = strings.TrimPrefix(m.Clones[0], m.Databases[0])
	}
	return &models.RestoreRecord{
		ID: m.RestoreID, TargetConnectionID: m.ConnectionID, Status: models.RestoreStatusInProgress, StartedAt: m.StartedAt,
		PITR: &models.PITRRestore{StreamID: m.StreamID, Clones: slices.Clone(m.Clones), CloneSuffix: suffix,
			InPlace: &models.PITRInPlace{Databases: slices.Clone(m.Databases), SafetyBackupIDs: slices.Clone(m.SafetyBackupIDs),
				StreamWasEnabled: m.StreamWasEnabled, KeepClone: m.KeepClone, StreamPaused: true, SwapStarted: true}},
	}
}
