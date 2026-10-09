package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// Outbox defaults.
const (
	// DefaultOutboxLimit caps the deliveries waiting in the outbox; the oldest are
	// dropped (and counted as dropped) beyond it.
	DefaultOutboxLimit = 10000
	// DefaultOutboxAttempts is how many rounds (each one SendWithRetry with the
	// retry policy) a delivery gets before it is given up.
	DefaultOutboxAttempts = 8
	// DefaultOutboxBackoff is the wait after the first failed round; it doubles
	// after every further one, up to DefaultOutboxMaxBackoff.
	DefaultOutboxBackoff = time.Minute
	// DefaultOutboxMaxBackoff caps the wait between rounds.
	DefaultOutboxMaxBackoff = time.Hour
	// outboxIdle is the longest the dispatcher sleeps without a wake-up.
	outboxIdle = time.Minute
	// outboxWriteTimeout bounds every outbox write.
	outboxWriteTimeout = 5 * time.Second
)

// OutboxEntry is one delivery waiting in the outbox: an event for a channel. It
// holds the event, never a rendered message or channel configuration, so no
// secret is stored with it; the message is rendered when it is sent.
type OutboxEntry struct {
	// ID orders the entries (ascending in queueing order); 0 before it is stored.
	ID int64
	// ChannelID and ChannelType name the channel.
	ChannelID   string
	ChannelType ChannelType
	// Event is the event to deliver.
	Event events.Event
	// Attempts counts the failed rounds.
	Attempts int
	// NextAttemptAt is when the delivery is due.
	NextAttemptAt time.Time
	// CreatedAt is when it was queued.
	CreatedAt time.Time
}

// Outbox is the durable delivery queue (implemented by *store.SQLiteStore).
// Deliveries are stored before they are sent and removed once delivered or given
// up, so a delivery interrupted by a crash is sent again after the restart (at
// least once; Event.ID lets receivers drop duplicates).
type Outbox interface {
	// EnqueueDeliveries stores entries in one transaction, then drops the oldest
	// entries beyond limit (no cap when limit <= 0) and returns them.
	EnqueueDeliveries(ctx context.Context, entries []OutboxEntry, limit int) (dropped []OutboxEntry, err error)
	// OutboxHeads returns the oldest entry of every channel, oldest first.
	OutboxHeads(ctx context.Context) ([]OutboxEntry, error)
	// DeleteDelivery removes entry id; a missing entry is not an error.
	DeleteDelivery(ctx context.Context, id int64) error
	// RetryDelivery records a failed round of entry id: its attempts, when it is
	// due again and the (redacted) error.
	RetryDelivery(ctx context.Context, id int64, attempts int, next time.Time, lastError string) error
}

// WithOutbox makes deliveries durable: every delivery is stored in o before it is
// sent and removed once it was delivered or given up; stored deliveries are sent
// (again) when Run starts. Deliveries of a channel are sent in order, one at a
// time. Without an outbox, deliveries are queued in memory only and lost when the
// process dies.
func WithOutbox(o Outbox) Option {
	return func(s *Service) { s.outbox = o }
}

// WithOutboxLimit caps the deliveries waiting in the outbox (DefaultOutboxLimit);
// non-positive values are ignored.
func WithOutboxLimit(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.outboxLimit = n
		}
	}
}

// WithOutboxRetry sets how many rounds a delivery in the outbox gets and the
// backoff between them (doubling from base up to maxBackoff). Non-positive
// values keep the defaults.
func WithOutboxRetry(attempts int, base, maxBackoff time.Duration) Option {
	return func(s *Service) {
		if attempts > 0 {
			s.outboxAttempts = attempts
		}
		if base > 0 {
			s.outboxBackoff = base
		}
		if maxBackoff > 0 {
			s.outboxMaxBackoff = maxBackoff
		}
	}
}

// persist stores e for every channel in channels and wakes the dispatcher. It
// reports false when the outbox cannot be written; the caller then queues the
// deliveries in memory.
func (s *Service) persist(ctx context.Context, e events.Event, channels []*Channel) bool {
	if len(channels) == 0 {
		return true
	}
	if e.ID == "" {
		id, err := newID("evt_")
		if err != nil {
			s.logger.Error("notification event ID unavailable", slog.Any("error", err))
			return false
		}
		e.ID = id
	}
	now := time.Now().UTC()
	entries := make([]OutboxEntry, 0, len(channels))
	for _, ch := range channels {
		entries = append(entries, OutboxEntry{ChannelID: ch.ID, ChannelType: ch.Type, Event: e, NextAttemptAt: now, CreatedAt: now})
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxWriteTimeout)
	defer cancel()
	dropped, err := s.outbox.EnqueueDeliveries(writeCtx, entries, s.outboxLimit)
	if err != nil {
		s.logger.Warn("notification outbox unavailable; delivering from memory, which a crash loses",
			slog.String("event", string(e.Type)), slog.Any("error", err))
		return false
	}
	for _, d := range dropped {
		s.logger.Warn("notification outbox full; the oldest delivery was dropped",
			slog.Int("limit", s.outboxLimit),
			slog.String("channel_id", d.ChannelID),
			slog.String("channel_type", string(d.ChannelType)),
			slog.String("event", string(d.Event.Type)),
		)
		s.observe(d.ChannelType, OutcomeDropped)
	}
	s.signal()
	return true
}

// signal wakes the dispatcher.
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dispatch hands the due outbox entries to the workers, the oldest entry of each
// channel at a time, until ctx ends.
func (s *Service) dispatch(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		}
		wait := s.dispatchDue(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// dispatchDue queues the due heads of the outbox whose channel has no delivery in
// flight, and returns how long to wait before the next check.
func (s *Service) dispatchDue(ctx context.Context) time.Duration {
	heads, err := s.outbox.OutboxHeads(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("notification outbox unreadable; retrying", slog.Any("error", err))
		}
		return outboxIdle
	}
	now := time.Now()
	wait := outboxIdle
	for _, h := range heads {
		if s.inFlight(h.ChannelID) {
			continue
		}
		if h.Event.Type == "" {
			s.discard(ctx, h, "unreadable")
			wait = 0
			continue
		}
		if until := h.NextAttemptAt.Sub(now); until > 0 {
			wait = min(wait, until)
			continue
		}
		ch, chErr := s.repo.GetChannel(ctx, h.ChannelID)
		switch {
		case errors.Is(chErr, ErrChannelNotFound):
			s.discard(ctx, h, "channel deleted")
			wait = 0
			continue
		case chErr != nil:
			s.logger.Warn("notification channel unavailable", slog.String("channel_id", h.ChannelID), slog.Any("error", chErr))
			continue
		case !ch.Enabled:
			s.discard(ctx, h, "channel disabled")
			wait = 0
			continue
		}
		s.setInFlight(h.ChannelID, true)
		d := delivery{channel: ch, msg: Render(h.Event), outboxID: h.ID, attempts: h.Attempts}
		select {
		case s.queue <- d:
		case <-ctx.Done():
			s.setInFlight(h.ChannelID, false)
			return 0
		}
	}
	return max(wait, time.Millisecond)
}

// discard removes outbox entry h that can no longer be delivered.
func (s *Service) discard(ctx context.Context, h OutboxEntry, reason string) {
	s.logger.Info("queued notification dropped",
		slog.String("channel_id", h.ChannelID), slog.String("event", string(h.Event.Type)), slog.String("reason", reason))
	if err := s.outbox.DeleteDelivery(ctx, h.ID); err != nil {
		s.logger.Warn("failed to remove a queued notification", slog.Any("error", err))
	}
}

// inFlight reports whether a delivery of channel id is being sent.
func (s *Service) inFlight(id string) bool {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	_, ok := s.flight[id]
	return ok
}

// setInFlight marks (or clears) a delivery of channel id as being sent.
func (s *Service) setInFlight(id string, on bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if on {
		s.flight[id] = struct{}{}
	} else {
		delete(s.flight, id)
	}
}

// settle records the outcome of outbox delivery d: delivered or given up removes
// it, a failed round schedules the next one with backoff. A round cut short by the
// end of the shutdown drain leaves it as it was, to be sent after the restart.
func (s *Service) settle(ctx context.Context, d delivery, sendErr error) {
	defer func() {
		s.setInFlight(d.channel.ID, false)
		s.signal()
	}()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxWriteTimeout)
	defer cancel()
	attempts := d.attempts + 1
	switch {
	case sendErr == nil:
		if err := s.outbox.DeleteDelivery(writeCtx, d.outboxID); err != nil {
			s.logger.Warn("failed to remove a delivered notification; it may be sent again", slog.Any("error", err))
		}
	case ctx.Err() != nil:
		// Shutdown: the delivery stays queued for the next start.
	case isPermanent(sendErr) || attempts >= s.outboxAttempts:
		s.logger.Warn("notification given up",
			slog.String("channel_id", d.channel.ID), slog.String("event", string(d.msg.Event.Type)), slog.Int("rounds", attempts))
		s.observe(d.channel.Type, OutcomeFailure)
		if err := s.outbox.DeleteDelivery(writeCtx, d.outboxID); err != nil {
			s.logger.Warn("failed to remove a notification that was given up", slog.Any("error", err))
		}
	default:
		next := time.Now().UTC().Add(s.outboxDelay(attempts))
		msg := truncate(singleLine(scrub(sendErr, secretsOf(d.channel)...).Error()), maxErrorLength)
		if err := s.outbox.RetryDelivery(writeCtx, d.outboxID, attempts, next, msg); err != nil {
			s.logger.Warn("failed to reschedule a notification", slog.Any("error", err))
		}
	}
}

// outboxDelay is the wait after failed round number attempts (1-based).
func (s *Service) outboxDelay(attempts int) time.Duration {
	d := s.outboxBackoff
	for i := 1; i < attempts && d < s.outboxMaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.outboxMaxBackoff)
}
