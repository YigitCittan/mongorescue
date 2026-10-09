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
// flight, and returns how long to wait before the next check. A head whose last
// outbox write failed follows its in-memory state (see heldDelivery): one that was
// delivered or given up is never sent again, only its removal is retried.
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
		if held, ok := s.heldState(h.ID); ok {
			if until := held.next.Sub(now); until > 0 {
				wait = min(wait, until)
				continue
			}
			if held.removeOnly {
				wait = min(wait, s.remove(ctx, h.ID))
				continue
			}
			h.Attempts, h.NextAttemptAt = held.attempts, held.next
		}
		if h.Event.Type == "" {
			wait = min(wait, s.discard(ctx, h, "unreadable"))
			continue
		}
		if until := h.NextAttemptAt.Sub(now); until > 0 {
			wait = min(wait, until)
			continue
		}
		ch, chErr := s.repo.GetChannel(ctx, h.ChannelID)
		switch {
		case errors.Is(chErr, ErrChannelNotFound):
			wait = min(wait, s.discard(ctx, h, "channel deleted"))
			continue
		case chErr != nil:
			s.logger.Warn("notification channel unavailable", slog.String("channel_id", h.ChannelID), slog.Any("error", chErr))
			continue
		case !ch.Enabled:
			wait = min(wait, s.discard(ctx, h, "channel disabled"))
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

// discard removes outbox entry h that can no longer be delivered and returns how
// long the dispatcher may wait before the channel's next entry (see remove).
func (s *Service) discard(ctx context.Context, h OutboxEntry, reason string) time.Duration {
	s.logger.Info("queued notification dropped",
		slog.String("channel_id", h.ChannelID), slog.String("event", string(h.Event.Type)), slog.String("reason", reason))
	return s.remove(ctx, h.ID)
}

// Backoff of a failed outbox write (see heldDelivery).
const (
	defaultHeldBackoff    = time.Second
	defaultHeldMaxBackoff = 5 * time.Minute
)

// heldDelivery is the in-memory state of an outbox entry whose last write failed,
// so the dispatcher never acts on the stale row in a tight loop. removeOnly marks
// an entry that was delivered (or given up or discarded): it is never sent again,
// only its removal is retried at next, with backoff. Otherwise attempts and next
// replace the row's, whose update failed.
type heldDelivery struct {
	removeOnly bool
	attempts   int
	next       time.Time
	backoff    time.Duration
}

// heldState returns the in-memory state of entry id, if any.
func (s *Service) heldState(id int64) (heldDelivery, bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	h, ok := s.held[id]
	if !ok {
		return heldDelivery{}, false
	}
	return *h, true
}

// release forgets the in-memory state of entry id.
func (s *Service) release(id int64) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	delete(s.held, id)
}

// remove deletes entry id. When the delete fails the entry is held for removal
// only, retried with a backoff doubling from one second to five minutes. It
// returns how long the dispatcher may wait before looking at the channel again.
func (s *Service) remove(ctx context.Context, id int64) time.Duration {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxWriteTimeout)
	defer cancel()
	err := s.outbox.DeleteDelivery(writeCtx, id)
	if err == nil {
		s.release(id)
		return 0
	}
	s.flightMu.Lock()
	h := s.held[id]
	if h == nil || !h.removeOnly {
		h = &heldDelivery{removeOnly: true}
		s.held[id] = h
	}
	if h.backoff == 0 {
		h.backoff = s.heldBackoff
	} else {
		h.backoff = min(2*h.backoff, s.heldMaxBackoff)
	}
	h.next = time.Now().Add(h.backoff)
	wait := h.backoff
	s.flightMu.Unlock()
	s.logger.Warn("failed to remove a sent or dropped notification from the outbox; it is not sent again, the removal is retried",
		slog.Int64("outbox_id", id), slog.Duration("retry_in", wait), slog.Any("error", err))
	return wait
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
// end of the shutdown drain leaves it as it was, to be sent after the restart. A
// write that fails is retried from memory (see heldDelivery), never by sending a
// delivered notification again.
func (s *Service) settle(ctx context.Context, d delivery, sendErr error) {
	defer func() {
		s.setInFlight(d.channel.ID, false)
		s.signal()
	}()
	attempts := d.attempts + 1
	switch {
	case sendErr == nil:
		s.remove(ctx, d.outboxID)
	case ctx.Err() != nil:
		// Shutdown: the delivery stays queued for the next start.
	case isPermanent(sendErr) || attempts >= s.outboxAttempts:
		s.giveUp(ctx, d.channel, d.outboxID, d.msg.Event.Type, attempts)
	default:
		msg := truncate(singleLine(scrub(sendErr, secretsOf(d.channel)...).Error()), maxErrorLength)
		s.reschedule(ctx, d.outboxID, attempts, msg)
	}
}

// giveUp ends the delivery of outbox entry id to ch after rounds failed rounds.
func (s *Service) giveUp(ctx context.Context, ch *Channel, id int64, event events.EventType, rounds int) {
	s.logger.Warn("notification given up",
		slog.String("channel_id", ch.ID), slog.String("event", string(event)), slog.Int("rounds", rounds))
	s.observe(ch.Type, OutcomeFailure)
	s.remove(ctx, id)
}

// reschedule records failed round number attempts of outbox entry id and when it
// is due again; when the write fails the new state is held in memory.
func (s *Service) reschedule(ctx context.Context, id int64, attempts int, lastError string) {
	next := time.Now().UTC().Add(s.outboxDelay(attempts))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxWriteTimeout)
	defer cancel()
	if err := s.outbox.RetryDelivery(writeCtx, id, attempts, next, lastError); err != nil {
		s.logger.Warn("failed to reschedule a notification; its next attempt is kept in memory",
			slog.Int64("outbox_id", id), slog.Any("error", err))
		s.flightMu.Lock()
		s.held[id] = &heldDelivery{attempts: attempts, next: next}
		s.flightMu.Unlock()
		return
	}
	s.release(id)
}

// outboxDelay is the wait after failed round number attempts (1-based).
func (s *Service) outboxDelay(attempts int) time.Duration {
	d := s.outboxBackoff
	for i := 1; i < attempts && d < s.outboxMaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.outboxMaxBackoff)
}
