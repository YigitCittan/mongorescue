package collector

import (
	"context"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// retryFailedBase is how long the schedule waits after a failed base backup
// before it tries again, whatever the cron schedule says.
const retryFailedBase = time.Hour

// maintain runs the base schedule and, when due, retention.
func (s *Service) maintain(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.runBaseSchedule(ctx)
	s.runChainTests(ctx)
	now := s.now()
	s.mu.Lock()
	due := now.Sub(s.lastRetention) >= s.cfg.RetentionInterval
	if due {
		s.lastRetention = now
	}
	s.mu.Unlock()
	if due {
		s.applyRetention(ctx)
	}
}

// runBaseSchedule starts the base backups that are due: a stream whose chain has
// started gets its first base at once, and then one per activation of its cron
// schedule after the newest base. A failed base is retried after an hour.
func (s *Service) runBaseSchedule(ctx context.Context) {
	if s.cfg.StartBase == nil || s.cfg.Bases == nil || s.cfg.NextRun == nil {
		return
	}
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, st := range streams {
		if !st.Enabled {
			continue
		}
		if _, err := s.cfg.Repo.LoadState(ctx, st.ID); err != nil {
			continue // the collector has not started the chain yet
		}
		bases, err := s.cfg.Bases(ctx, st.ID)
		if err != nil {
			s.logger.Warn("cannot list the base backups of a PITR stream", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
			continue
		}
		status, err := s.Status(ctx, st.ID)
		if err != nil {
			s.logger.Warn("cannot read the status of a PITR stream", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
			continue
		}
		if !s.baseDue(st, status, bases, now) {
			continue
		}
		rec, err := s.cfg.StartBase(ctx, st.ID, models.TriggerScheduled)
		if err != nil {
			// Typically another base of the connection is running (busy): the
			// schedule asks again at its next check, after that one.
			s.logger.Warn("cannot start a PITR base backup; the schedule retries it", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
			continue
		}
		s.logger.Info("scheduled PITR base backup started", logsafe.Attr("stream_id", st.ID), logsafe.Attr("backup_id", rec.ID))
	}
}

// runChainTests starts the chain tests that are due: one per activation of a
// stream's chain_test_cron, counted from its newest chain test (or from the last
// attempt, so a stream without two bases to test is not asked again every check).
func (s *Service) runChainTests(ctx context.Context) {
	if s.cfg.StartChainTest == nil || s.cfg.LastChainTest == nil || s.cfg.NextRun == nil {
		return
	}
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, st := range streams {
		if !st.Enabled || st.ChainTestCron == "" {
			continue
		}
		from := s.cfg.LastChainTest(ctx, st.ID)
		s.mu.Lock()
		if s.chainTestTried == nil {
			s.chainTestTried = map[string]time.Time{}
		}
		if tried := s.chainTestTried[st.ID]; tried.After(from) {
			from = tried
		}
		s.mu.Unlock()
		if from.IsZero() {
			from = st.CreatedAt
		}
		next, ok := s.cfg.NextRun(st.ChainTestCron, from)
		if !ok || now.Before(next) {
			continue
		}
		s.mu.Lock()
		s.chainTestTried[st.ID] = now
		s.mu.Unlock()
		if err := s.cfg.StartChainTest(ctx, st.ID); err != nil {
			s.logger.Info("scheduled PITR chain test not started", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
			continue
		}
		s.logger.Info("scheduled PITR chain test started", logsafe.Attr("stream_id", st.ID))
	}
}

// baseDue reports whether stream st needs a base backup at now; status is its
// status and bases its bases, newest first.
//
// A base is due at once when the current chain has no open window (a new
// stream, a chain started after a gap or a divergence, or one split by a chunk
// that failed verification), unless a base is running or the newest one waits
// for the chain to reach its T_after; a failed base is retried after an hour.
// Otherwise the cron schedule decides, counted from the newest base.
func (s *Service) baseDue(st *pitr.Stream, status *StreamStatus, bases []*models.BackupRecord, now time.Time) bool {
	var newest *models.BackupRecord
	for _, b := range bases {
		// A skipped run never started; a deleted base does not count.
		if !b.Status.Deleted() && b.Status != models.StatusSkipped {
			newest = b
			break
		}
	}
	switch {
	case newest == nil:
		return true
	case newest.Status == models.StatusInProgress || newest.Status == models.StatusPending:
		// Running, or waiting for a slot of the connection's max_concurrent_backups.
		return false
	case newest.Status == models.StatusCancelled && newest.CancelledBy == runs.SystemActor:
		// Interrupted by a shutdown while it waited for a slot: it never ran.
		return !hasOpenWindow(status)
	case newest.Status == models.StatusFailed || newest.Status == models.StatusCancelled:
		if now.Sub(newest.StartedAt) >= retryFailedBase {
			return true
		}
	case !hasOpenWindow(status):
		return !awaitsCoverage(status, newest)
	}
	next, ok := s.cfg.NextRun(st.BaseCron, newest.StartedAt)
	return ok && !now.Before(next)
}

// hasOpenWindow reports whether the stream's current chain has a growing window.
func hasOpenWindow(status *StreamStatus) bool {
	for _, w := range status.Windows {
		if w.Open {
			return true
		}
	}
	return false
}

// awaitsCoverage reports whether completed base b belongs to the current chain
// but the chain has not reached its T_after yet: it becomes eligible by itself.
func awaitsCoverage(status *StreamStatus, b *models.BackupRecord) bool {
	if b.Status != models.StatusCompleted || b.TBefore == nil || b.TAfter == nil {
		return false
	}
	for _, c := range status.Chains {
		if !c.Open() || len(c.Segments) == 0 {
			continue
		}
		last := c.Segments[len(c.Segments)-1]
		return last.To == c.lastTo && last.From.Compare(b.TBefore.TS) <= 0 && last.To.Compare(b.TAfter.TS) < 0
	}
	return false
}

// TakeBase starts a base backup of stream id now. Expected failures: those of
// the configured BaseStarter, and ErrUnavailable without one.
func (s *Service) TakeBase(ctx context.Context, id string, trigger models.BackupTrigger) (*models.BackupRecord, error) {
	if s.cfg.StartBase == nil {
		return nil, ErrUnavailable
	}
	if _, err := s.cfg.Repo.GetStream(ctx, id); err != nil {
		return nil, err
	}
	rec, err := s.cfg.StartBase(ctx, id, trigger)
	if err != nil {
		return nil, err
	}
	return rec, nil
}
