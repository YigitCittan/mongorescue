package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Thresholds of the lag and headroom alerts.
const (
	// minLagThreshold is the smallest lag that raises pitr.lag_high; the threshold
	// is max(minLagThreshold, lagIntervals x the chunk interval).
	minLagThreshold = 5 * time.Minute
	lagIntervals    = 5
	// minHeadroom is the smallest headroom that does not raise pitr.window_low; the
	// threshold is max(minHeadroom, 3 x lag).
	minHeadroom = 6 * time.Hour
	// behindRetries is how many range reads in a row may end before their upper
	// bound (a lagging member) before the collector counts it as a failure.
	behindRetries = 3
	// behindDelay is the wait before such a read is repeated.
	behindDelay = time.Second
	// maxFailureDelay caps the wait after a failed tick.
	maxFailureDelay = time.Minute
)

// LagThreshold returns the lag above which a stream with chunk interval interval
// raises pitr.lag_high.
func LagThreshold(interval time.Duration) time.Duration {
	return max(minLagThreshold, lagIntervals*interval)
}

// HeadroomThreshold returns the headroom below which a stream lagging by lag
// raises pitr.window_low: max(6h, 3 x lag).
func HeadroomThreshold(lag time.Duration) time.Duration {
	return max(minHeadroom, 3*lag)
}

// Live is what a running collector knows beyond the stored state.
type Live struct {
	// Lag is how far the collector's position is behind the replica set's newest
	// write; Headroom how far it is ahead of the oldest oplog entry.
	Lag      time.Duration `json:"lag_seconds"`
	Headroom time.Duration `json:"headroom_seconds"`
	// Oldest and Newest are the oplog window of the member last read.
	Oldest pitr.Timestamp `json:"oldest"`
	Newest pitr.Timestamp `json:"newest"`
	// Interval is the chunk interval in force (halved after oversized chunks).
	Interval time.Duration `json:"interval_seconds"`
	// CatchingUp reports that the last chunk was capped at one interval.
	CatchingUp bool `json:"catching_up"`
	// WindowLow reports that the headroom is below its threshold.
	WindowLow bool `json:"window_low"`
	// LastTick is when the collector last looked at the oplog.
	LastTick time.Time `json:"last_tick"`
}

// worker collects the oplog of one stream.
type worker struct {
	svc    *Service
	stream *pitr.Stream
	sess   Session

	// interval is the chunk interval in force.
	interval time.Duration
	// checked is set once the stored position was checked for divergence.
	checked bool
	// inclusive makes the next chunk keep the entry at its start (the first chunk
	// of a chain).
	inclusive bool
	// rsID is the replica set ID first seen.
	rsID string
	// failing, lagSince and windowLow track the alerts already raised.
	failing   bool
	lagSince  *time.Time
	windowLow bool
	// behind counts range reads in a row that ended before their bound.
	behind int
	// emptyAt is the position an empty chunk was last written at.
	emptyAt pitr.Timestamp

	mu   sync.Mutex
	live Live
}

// newWorker returns the worker of stream st.
func newWorker(s *Service, st *pitr.Stream) *worker {
	secs := st.ChunkSeconds
	if secs <= 0 {
		secs = DefaultChunkSeconds
	}
	return &worker{svc: s, stream: st, interval: time.Duration(secs) * time.Second}
}

// configured returns the stream's chunk interval.
func (w *worker) configured() time.Duration {
	secs := w.stream.ChunkSeconds
	if secs <= 0 {
		secs = DefaultChunkSeconds
	}
	return time.Duration(secs) * time.Second
}

// snapshot returns the live state.
func (w *worker) snapshot() Live {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live
}

// run collects until ctx ends.
func (w *worker) run(ctx context.Context) {
	defer func() {
		if w.sess != nil {
			w.sess.Close()
			w.sess = nil
		}
	}()
	w.svc.logger.Info("PITR collector started", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("connection_id", w.stream.ConnectionID))
	for {
		delay := w.tick(ctx)
		if ctx.Err() != nil {
			w.svc.logger.Info("PITR collector stopped", logsafe.Attr("stream_id", w.stream.ID))
			return
		}
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			w.svc.logger.Info("PITR collector stopped", logsafe.Attr("stream_id", w.stream.ID))
			return
		case <-w.svc.cfg.Clock.After(delay):
		}
	}
}

// tick runs one step and handles its outcome; it returns the wait before the next
// step.
func (w *worker) tick(ctx context.Context) time.Duration {
	delay, err := w.step(ctx)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		w.fail(ctx, err)
		return min(w.interval, maxFailureDelay)
	}
	w.recover(ctx)
	return delay
}

// fail records a failed step: the first failure of an episode raises
// pitr.collector_failed.
func (w *worker) fail(ctx context.Context, err error) {
	msg := redact.Text(err.Error())
	w.svc.observe(func(o Observer) { o.SetPITRCollectorUp(w.stream.ID, false) })
	if setErr := w.svc.cfg.Repo.SetCollectorStatus(ctx, w.stream.ID, pitr.CollectorFailed, msg, w.lagSince, w.svc.now()); setErr != nil &&
		!errors.Is(setErr, pitr.ErrNotFound) {
		w.svc.logger.Warn("cannot record the PITR collector's failure", logsafe.Attr("stream_id", w.stream.ID), logsafe.Error(setErr))
	}
	if w.failing {
		w.svc.logger.Debug("PITR collector still failing", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("error", msg))
		return
	}
	w.failing = true
	w.svc.logger.Warn("PITR collector failing", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("error", msg))
	w.svc.publish(ctx, w.event(events.PITRCollectorFailed, "failed", msg, ""))
}

// recover records a successful step after failures.
func (w *worker) recover(ctx context.Context) {
	w.svc.observe(func(o Observer) { o.SetPITRCollectorUp(w.stream.ID, true) })
	if !w.failing {
		return
	}
	w.failing = false
	if err := w.svc.cfg.Repo.SetCollectorStatus(ctx, w.stream.ID, pitr.CollectorRunning, "", w.lagSince, w.svc.now()); err != nil {
		w.svc.logger.Warn("cannot record the PITR collector's recovery", logsafe.Attr("stream_id", w.stream.ID), logsafe.Error(err))
	}
	w.svc.logger.Info("PITR collector recovered", logsafe.Attr("stream_id", w.stream.ID))
	w.svc.publish(ctx, w.event(events.PITRCollectorRecovered, "running", "", ""))
}

// event returns a pitr.* event of the stream.
func (w *worker) event(t events.EventType, status, errMsg, detail string) events.Event {
	return events.Event{Type: t, Time: w.svc.now(), Status: status, Error: redact.Text(errMsg), Detail: redact.Text(detail),
		Stream: w.stream.ID, ConnectionID: w.stream.ConnectionID}
}

// errRetry asks for the step to be repeated after a short wait without counting
// as a failure.
var errRetry = errors.New("collector: retry")

// step reads and stores the next chunk, or handles a gap or a divergence. It
// returns the wait before the next step: 0 while catching up.
func (w *worker) step(ctx context.Context) (time.Duration, error) {
	if w.sess == nil {
		sess, err := w.svc.cfg.Open(ctx, w.stream)
		if err != nil {
			return 0, fmt.Errorf("connect to the replica set: %w", err)
		}
		w.sess = sess
	}
	win, err := w.sess.OplogWindow(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the oplog window: %w", err)
	}
	now := w.svc.now()
	w.mu.Lock()
	w.live.LastTick, w.live.Oldest, w.live.Newest, w.live.Interval = now, win.Oldest, win.Newest, w.interval
	w.mu.Unlock()

	st, err := w.svc.cfg.Repo.LoadState(ctx, w.stream.ID)
	if errors.Is(err, pitr.ErrNotFound) {
		// A new stream starts at the newest majority-committed entry.
		w.rsID = win.ReplicaSetID
		return 0, w.startChain(ctx, win.MajorityOpTime)
	}
	if err != nil {
		return 0, fmt.Errorf("load the collector state: %w", err)
	}
	if !w.checked {
		w.lagSince = st.LagSince
		w.failing = w.failing || st.Status == pitr.CollectorFailed
		if done, err := w.checkStart(ctx, st, win); err != nil || done {
			return 0, err
		}
	}

	// A changed replica set ends the chain: its oplog is another history.
	if win.ReplicaSet != w.stream.ReplicaSet || (w.rsID != "" && win.ReplicaSetID != "" && win.ReplicaSetID != w.rsID) {
		detail := fmt.Sprintf("the replica set changed from %s (%s) to %s (%s)", w.stream.ReplicaSet, w.rsID, win.ReplicaSet, win.ReplicaSetID)
		return 0, w.breakChain(ctx, st, win, pitr.EndReplicaSetChanged, detail)
	}
	if w.rsID == "" {
		w.rsID = win.ReplicaSetID
	}
	// The window was overrun: entries after the position are lost.
	if win.Oldest.Compare(st.Last.TS) > 0 {
		detail := fmt.Sprintf("the oplog window was overrun: the collector was at %s, the oldest entry is %s", st.Last.TS, win.Oldest)
		return 0, w.breakChain(ctx, st, win, pitr.EndGap, detail)
	}

	a, b := st.Last, win.MajorityOpTime.TS
	w.observeLag(ctx, st, win, now)
	if b.Compare(a.TS) < 0 {
		// The member's majority view is behind the stored position.
		return w.interval, nil
	}
	to, catchUp := b, false
	if span := time.Duration(b.T-a.TS.T) * time.Second; span > w.interval {
		capAt := pitr.Timestamp{T: a.TS.T + uint32(w.interval/time.Second)} //nolint:gosec // the interval is at most 900 s
		op, found, err := w.sess.EntryAtOrAfter(ctx, capAt)
		if err != nil {
			return 0, fmt.Errorf("find the end of a catch-up chunk: %w", err)
		}
		if found && op.TS.Compare(b) < 0 && op.TS.Compare(a.TS) > 0 {
			to, catchUp = op.TS, true
		}
	}
	w.mu.Lock()
	w.live.CatchingUp = catchUp
	w.mu.Unlock()

	if to == a.TS && !w.inclusive {
		// Nothing new: one empty chunk proves the position was looked at.
		if w.emptyAt == a.TS {
			return w.interval, nil
		}
		exists, err := w.emptyChunkExists(ctx, st)
		if err != nil {
			return 0, err
		}
		if exists {
			w.emptyAt = a.TS
			return w.interval, nil
		}
	}

	rng := pitr.OplogRange{From: a.TS, To: to, CheckTerm: true, FromTerm: a.Term, StartInclusive: w.inclusive}
	chunk, rawBytes, err := w.writeChunk(ctx, st.ChainID, rng, a.Term)
	switch {
	case errors.Is(err, pitr.ErrOplogBehind):
		w.behind++
		if w.behind < behindRetries {
			return behindDelay, nil
		}
		w.behind = 0
		return 0, err
	case errors.Is(err, pitr.ErrOplogGap):
		return 0, w.recheck(ctx, st, err)
	case err != nil:
		w.svc.observe(func(o Observer) { o.ObservePITRChunk(w.stream.ID, false, 0, time.Time{}) })
		return 0, err
	}
	w.behind = 0
	w.inclusive = false
	if chunk.Entries == 0 {
		w.emptyAt = chunk.To
	}
	w.svc.observe(func(o Observer) { o.ObservePITRChunk(w.stream.ID, true, chunk.SizeBytes, chunk.To.Time()) })
	w.adjustInterval(rawBytes)
	if catchUp {
		return 0, nil
	}
	return w.interval, nil
}

// adjustInterval halves the interval after a chunk above MaxChunkBytes and
// doubles it back, up to the configured one, after a chunk below a quarter of it.
func (w *worker) adjustInterval(rawBytes int64) {
	limit := w.svc.cfg.MaxChunkBytes
	switch {
	case rawBytes > limit && w.interval > time.Second:
		w.interval = max(w.interval/2, time.Second)
		w.svc.logger.Info("PITR chunk above the size limit; halving the chunk interval",
			logsafe.Attr("stream_id", w.stream.ID), slog.Int64("bytes", rawBytes), slog.Duration("interval", w.interval))
	case rawBytes < limit/4 && w.interval < w.configured():
		w.interval = min(w.interval*2, w.configured())
	}
}

// emptyChunkExists reports whether the chain already has the empty chunk at the
// stored position (written before a restart).
func (w *worker) emptyChunkExists(ctx context.Context, st *pitr.State) (bool, error) {
	chunks, err := w.svc.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: w.stream.ID, ChainID: st.ChainID, After: prev(st.Last.TS)})
	if err != nil {
		return false, fmt.Errorf("list the chain's last chunk: %w", err)
	}
	for _, c := range chunks {
		if c.From == st.Last.TS && c.To == st.Last.TS {
			return true, nil
		}
	}
	return false, nil
}

// prev returns the timestamp just before ts.
func prev(ts pitr.Timestamp) pitr.Timestamp {
	switch {
	case ts.I > 0:
		return pitr.Timestamp{T: ts.T, I: ts.I - 1}
	case ts.T > 0:
		return pitr.Timestamp{T: ts.T - 1, I: ^uint32(0)}
	default:
		return ts
	}
}

// checkStart checks the stored position once at start: the entry there must
// still exist with the stored term, or the chain ended in a gap or a divergence.
// It also decides whether the next chunk is the first of its chain. done reports
// that the chain was ended and a new one started.
func (w *worker) checkStart(ctx context.Context, st *pitr.State, win pitr.OplogWindow) (done bool, err error) {
	term, found, err := w.sess.EntryAt(ctx, st.Last.TS)
	if err != nil {
		return false, fmt.Errorf("check the stored position: %w", err)
	}
	w.rsID = win.ReplicaSetID
	switch {
	case !found && win.Oldest.Compare(st.Last.TS) > 0:
		detail := fmt.Sprintf("the oplog window was overrun while the collector was stopped: it was at %s, the oldest entry is %s", st.Last.TS, win.Oldest)
		return true, w.breakChain(ctx, st, win, pitr.EndGap, detail)
	case !found || term != st.Last.Term:
		return true, w.diverge(ctx, st, win)
	}
	chunks, err := w.svc.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: w.stream.ID, ChainID: st.ChainID, After: prev(st.Last.TS)})
	if err != nil {
		return false, fmt.Errorf("list the chain's last chunk: %w", err)
	}
	w.inclusive = len(chunks) == 0 && w.chainStartsAt(ctx, st)
	w.checked = true
	return false, nil
}

// chainStartsAt reports whether the state's chain starts at the stored position
// (no chunk was committed to it yet).
func (w *worker) chainStartsAt(ctx context.Context, st *pitr.State) bool {
	chains, err := w.svc.cfg.Repo.ListChains(ctx, w.stream.ID)
	if err != nil {
		return false
	}
	for _, c := range chains {
		if c.ChainID == st.ChainID {
			return c.Start == st.Last.TS
		}
	}
	return false
}

// recheck decides after a range read failed with pitr.ErrOplogGap whether the
// window was overrun or the history diverged; when the start entry is there again
// the failure was transient.
func (w *worker) recheck(ctx context.Context, st *pitr.State, readErr error) error {
	term, found, err := w.sess.EntryAt(ctx, st.Last.TS)
	if err != nil {
		return fmt.Errorf("re-check the position after %w: %w", readErr, err)
	}
	win, err := w.sess.OplogWindow(ctx)
	if err != nil {
		return fmt.Errorf("re-check the window after %w: %w", readErr, err)
	}
	switch {
	case !found && win.Oldest.Compare(st.Last.TS) > 0:
		detail := fmt.Sprintf("the oplog window was overrun: the collector was at %s, the oldest entry is %s", st.Last.TS, win.Oldest)
		return w.breakChain(ctx, st, win, pitr.EndGap, detail)
	case !found || term != st.Last.Term:
		return w.diverge(ctx, st, win)
	default:
		return readErr
	}
}

// startChain starts a new chain at start; its first chunk keeps the entry there.
func (w *worker) startChain(ctx context.Context, start pitr.OpTime) error {
	id := newChainID(w.svc.now())
	if err := w.svc.cfg.Repo.StartChain(ctx, w.stream.ID, id, start, w.svc.now()); err != nil {
		return fmt.Errorf("start a chain: %w", err)
	}
	w.inclusive, w.checked, w.emptyAt = true, true, pitr.Timestamp{}
	w.svc.logger.Info("PITR chain started", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("chain_id", id),
		logsafe.Attr("start", start.TS.String()))
	return nil
}

// newChainID returns a chain ID that sorts by its start time.
func newChainID(at time.Time) string {
	return newID("ch_" + at.UTC().Format("20060102T150405Z") + "_")
}

// startAtOldest starts a new chain at the oldest entry of the window.
func (w *worker) startAtOldest(ctx context.Context, win pitr.OplogWindow) error {
	term, found, err := w.sess.EntryAt(ctx, win.Oldest)
	if err != nil {
		return fmt.Errorf("read the oldest oplog entry: %w", err)
	}
	if !found {
		return fmt.Errorf("the oldest oplog entry %s vanished; retrying", win.Oldest)
	}
	return w.startChain(ctx, pitr.OpTime{TS: win.Oldest, Term: term})
}

// breakChain ends the chain for a gap or a replica set change, starts a new one at
// the oldest entry, takes a base backup (Stream.BaseOnGap) and raises
// pitr.chain_broken.
func (w *worker) breakChain(ctx context.Context, st *pitr.State, win pitr.OplogWindow, reason pitr.EndReason, detail string) error {
	if err := w.svc.cfg.Repo.EndChain(ctx, w.stream.ID, st.ChainID, st.Last.TS, reason, w.svc.now()); err != nil && !errors.Is(err, pitr.ErrChainEnded) {
		return fmt.Errorf("end the chain: %w", err)
	}
	w.svc.logger.Error("PITR chain broken", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("chain_id", st.ChainID),
		logsafe.Attr("reason", string(reason)), logsafe.Attr("detail", detail))
	w.svc.observe(func(o Observer) { o.IncPITRChainBreak(w.stream.ID, string(reason)) })
	if reason == pitr.EndReplicaSetChanged && win.ReplicaSet != "" && win.ReplicaSet != w.stream.ReplicaSet {
		// The stream follows the replica set its connection reaches now.
		updated := *w.stream
		updated.ReplicaSet = win.ReplicaSet
		if err := w.svc.cfg.Repo.UpdateStream(ctx, &updated); err != nil {
			return fmt.Errorf("record the new replica set: %w", err)
		}
		w.stream.ReplicaSet, w.stream.UpdatedAt = updated.ReplicaSet, updated.UpdatedAt
	}
	w.rsID = win.ReplicaSetID
	w.svc.publish(ctx, w.event(events.PITRChainBroken, string(reason), "", detail))
	if err := w.startAtOldest(ctx, win); err != nil {
		return err
	}
	w.baseAfterBreak(ctx)
	return nil
}

// diverge ends the chain at the newest chunk whose end entry still exists with its
// term, supersedes the chunks after it, starts a new chain and raises
// pitr.diverged.
func (w *worker) diverge(ctx context.Context, st *pitr.State, win pitr.OplogWindow) error {
	point, err := w.divergencePoint(ctx, st, win)
	if err != nil {
		return err
	}
	n, err := w.svc.cfg.Repo.SupersedeChunks(ctx, w.stream.ID, st.ChainID, point)
	if err != nil {
		return fmt.Errorf("supersede the chunks after the divergence: %w", err)
	}
	if err := w.svc.cfg.Repo.EndChain(ctx, w.stream.ID, st.ChainID, point, pitr.EndDiverged, w.svc.now()); err != nil && !errors.Is(err, pitr.ErrChainEnded) {
		return fmt.Errorf("end the diverged chain: %w", err)
	}
	detail := fmt.Sprintf("the oplog entry at %s (term %d) vanished or changed term; the chain ends at %s and %d later chunk(s) are superseded",
		st.Last.TS, st.Last.Term, point, n)
	w.svc.logger.Error("PITR oplog diverged", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("chain_id", st.ChainID), logsafe.Attr("detail", detail))
	w.svc.observe(func(o Observer) { o.IncPITRChainBreak(w.stream.ID, string(pitr.EndDiverged)) })
	w.svc.publish(ctx, w.event(events.PITRDiverged, string(pitr.EndDiverged), "", detail))
	w.rsID = win.ReplicaSetID
	term, found, err := w.sess.EntryAt(ctx, point)
	switch {
	case err != nil:
		return fmt.Errorf("read the divergence point: %w", err)
	case found:
		err = w.startChain(ctx, pitr.OpTime{TS: point, Term: term})
	default:
		err = w.startAtOldest(ctx, win)
	}
	if err != nil {
		return err
	}
	w.baseAfterBreak(ctx)
	return nil
}

// divergencePoint returns the end of the newest committed chunk of the state's
// chain whose end entry still exists with its term, or the chain's start. Chunks
// that end before the oldest entry cannot be checked and count as good.
func (w *worker) divergencePoint(ctx context.Context, st *pitr.State, win pitr.OplogWindow) (pitr.Timestamp, error) {
	chunks, err := w.svc.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: w.stream.ID, ChainID: st.ChainID, Status: pitr.ChunkCommitted})
	if err != nil {
		return pitr.Timestamp{}, fmt.Errorf("list the chain's chunks: %w", err)
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		c := chunks[i]
		if c.To.Compare(win.Oldest) < 0 {
			return c.To, nil
		}
		term, found, err := w.sess.EntryAt(ctx, c.To)
		if err != nil {
			return pitr.Timestamp{}, fmt.Errorf("check chunk %s: %w", c.ID, err)
		}
		if found && term == c.LastTerm {
			return c.To, nil
		}
	}
	chains, err := w.svc.cfg.Repo.ListChains(ctx, w.stream.ID)
	if err != nil {
		return pitr.Timestamp{}, fmt.Errorf("list the chains: %w", err)
	}
	for _, c := range chains {
		if c.ChainID == st.ChainID {
			return c.Start, nil
		}
	}
	return st.Last.TS, nil
}

// baseAfterBreak takes a base backup for the new chain (Stream.BaseOnGap).
func (w *worker) baseAfterBreak(ctx context.Context) {
	if !w.stream.BaseOnGap || w.svc.cfg.StartBase == nil {
		return
	}
	rec, err := w.svc.cfg.StartBase(ctx, w.stream.ID, models.TriggerScheduled)
	if err != nil {
		w.svc.logger.Warn("cannot start the base backup of a new PITR chain", logsafe.Attr("stream_id", w.stream.ID), logsafe.Error(err))
		return
	}
	w.svc.logger.Info("base backup of a new PITR chain started", logsafe.Attr("stream_id", w.stream.ID), logsafe.Attr("backup_id", rec.ID))
}

// observeLag updates the lag and headroom, their metrics and alerts.
func (w *worker) observeLag(ctx context.Context, st *pitr.State, win pitr.OplogWindow, now time.Time) {
	newest := win.Newest
	if win.MajorityOpTime.TS.Compare(newest) > 0 {
		newest = win.MajorityOpTime.TS
	}
	lag := time.Duration(0)
	if newest.T > st.Last.TS.T {
		lag = time.Duration(newest.T-st.Last.TS.T) * time.Second
	}
	headroom := time.Duration(0)
	if st.Last.TS.T > win.Oldest.T {
		headroom = time.Duration(st.Last.TS.T-win.Oldest.T) * time.Second
	}
	low := headroom < HeadroomThreshold(lag)
	w.mu.Lock()
	w.live.Lag, w.live.Headroom, w.live.WindowLow = lag, headroom, low
	w.mu.Unlock()
	w.svc.observe(func(o Observer) { o.SetPITRLag(w.stream.ID, lag, headroom) })

	switch high := lag > LagThreshold(w.configured()); {
	case high && w.lagSince == nil:
		since := now
		w.lagSince = &since
		w.setLagSince(ctx, st)
		w.svc.publish(ctx, w.event(events.PITRLagHigh, "lagging", "",
			fmt.Sprintf("the collector is %s behind the replica set (threshold %s)", lag, LagThreshold(w.configured()))))
	case !high && w.lagSince != nil:
		w.lagSince = nil
		w.setLagSince(ctx, st)
		w.svc.publish(ctx, w.event(events.PITRLagRecovered, "running", "", fmt.Sprintf("the collector is %s behind the replica set", lag)))
	}
	switch {
	case low && !w.windowLow:
		w.windowLow = true
		w.svc.publish(ctx, w.event(events.PITRWindowLow, "window_low", "",
			fmt.Sprintf("the oplog holds %s before the collector's position (threshold %s)", headroom, HeadroomThreshold(lag))))
	case !low:
		w.windowLow = false
	}
}

// setLagSince persists the lag episode, so a restart does not raise it again.
func (w *worker) setLagSince(ctx context.Context, st *pitr.State) {
	status := st.Status
	if status == "" || status == pitr.CollectorStopped {
		status = pitr.CollectorRunning
	}
	if err := w.svc.cfg.Repo.SetCollectorStatus(ctx, w.stream.ID, status, st.LastError, w.lagSince, w.svc.now()); err != nil {
		w.svc.logger.Warn("cannot record the PITR lag", logsafe.Attr("stream_id", w.stream.ID), logsafe.Error(err))
	}
}
