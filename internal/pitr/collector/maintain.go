package collector

import (
	"context"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
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
		if !s.baseDue(st, bases, now) {
			continue
		}
		rec, err := s.cfg.StartBase(ctx, st.ID, models.TriggerScheduled)
		if err != nil {
			s.logger.Warn("cannot start a scheduled PITR base backup", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
			continue
		}
		s.logger.Info("scheduled PITR base backup started", logsafe.Attr("stream_id", st.ID), logsafe.Attr("backup_id", rec.ID))
	}
}

// baseDue reports whether stream st needs a base backup at now; bases are its
// bases, newest first.
func (s *Service) baseDue(st *pitr.Stream, bases []*models.BackupRecord, now time.Time) bool {
	var newest *models.BackupRecord
	for _, b := range bases {
		if !b.Status.Deleted() {
			newest = b
			break
		}
	}
	switch {
	case newest == nil:
		return true
	case newest.Status == models.StatusInProgress || newest.Status == models.StatusPending:
		return false
	case newest.Status == models.StatusFailed || newest.Status == models.StatusCancelled:
		if now.Sub(newest.StartedAt) >= retryFailedBase {
			return true
		}
	}
	next, ok := s.cfg.NextRun(st.BaseCron, newest.StartedAt)
	return ok && !now.Before(next)
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
