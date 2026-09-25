package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// Service defaults.
const (
	// DefaultWorkers is the default number of concurrent deliveries.
	DefaultWorkers = 4
	// DefaultQueueSize is the default capacity of the delivery queue.
	DefaultQueueSize = 128
	// DefaultDrainTimeout bounds how long queued deliveries keep being sent after
	// shutdown begins.
	DefaultDrainTimeout = 15 * time.Second
	// statusPersistTimeout bounds delivery-status writes.
	statusPersistTimeout = 5 * time.Second
)

// Delivery outcome labels reported to the observer (and used as metric labels).
const (
	// OutcomeSuccess means the provider accepted the message.
	OutcomeSuccess = "success"
	// OutcomeFailure means every attempt failed.
	OutcomeFailure = "failure"
	// OutcomeDropped means the delivery queue was full or the service had stopped.
	OutcomeDropped = "dropped"
)

// ErrServiceRunning is returned by Run when the Service is already running or has run.
var ErrServiceRunning = errors.New("notify: service already started")

// Observer receives one call per finished (or dropped) delivery.
type Observer func(channelType ChannelType, outcome string)

// NotifierFactory builds the Notifier for a channel.
type NotifierFactory func(ch *Channel) (Notifier, error)

// delivery is one queued channel send.
type delivery struct {
	channel *Channel
	msg     Message
}

// Service manages channels and rules and dispatches events to channels. Construct it
// with NewService, subscribe HandleEvent to the events Bus and run Run in a goroutine
// owned by the application lifecycle.
type Service struct {
	repo    Repository
	logger  *slog.Logger
	factory NotifierFactory

	httpClient      *http.Client
	telegramBaseURL string
	twilioBaseURL   string
	tlsConfig       *tls.Config

	retry        RetryPolicy
	workers      int
	queue        chan delivery
	drainTimeout time.Duration
	observer     Observer

	// mu serialises configuration mutations (check-then-write sequences).
	mu sync.Mutex

	started atomic.Bool
	stopped atomic.Bool
}

// Option customises a Service.
type Option func(*Service)

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithHTTPClient sets the client used by webhook, Telegram and Twilio notifiers.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// WithTelegramBaseURL overrides the Telegram Bot API root (tests, self-hosted proxies).
func WithTelegramBaseURL(u string) Option {
	return func(s *Service) { s.telegramBaseURL = u }
}

// WithTwilioBaseURL overrides the Twilio API root (tests).
func WithTwilioBaseURL(u string) Option {
	return func(s *Service) { s.twilioBaseURL = u }
}

// WithTLSConfig sets the TLS configuration used for SMTP connections.
func WithTLSConfig(c *tls.Config) Option {
	return func(s *Service) { s.tlsConfig = c }
}

// WithRetryPolicy overrides the delivery retry policy.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(s *Service) { s.retry = p }
}

// WithWorkers sets the number of concurrent deliveries. Non-positive values are ignored.
func WithWorkers(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.workers = n
		}
	}
}

// WithQueueSize sets the delivery queue capacity. Non-positive values are ignored.
func WithQueueSize(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.queue = make(chan delivery, n)
		}
	}
}

// WithDrainTimeout bounds how long queued deliveries are still sent after shutdown.
func WithDrainTimeout(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.drainTimeout = d
		}
	}
}

// WithObserver registers a callback invoked for every delivery outcome.
func WithObserver(o Observer) Option {
	return func(s *Service) { s.observer = o }
}

// WithNotifierFactory replaces the built-in notifier construction (tests).
func WithNotifierFactory(f NotifierFactory) Option {
	return func(s *Service) { s.factory = f }
}

// NewService constructs a Service backed by repo.
func NewService(repo Repository, opts ...Option) *Service {
	s := &Service{
		repo:         repo,
		logger:       slog.Default(),
		retry:        DefaultRetryPolicy,
		workers:      DefaultWorkers,
		queue:        make(chan delivery, DefaultQueueSize),
		drainTimeout: DefaultDrainTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.httpClient == nil {
		s.httpClient = NewHTTPClient()
	}
	if s.factory == nil {
		s.factory = s.buildNotifier
	}
	return s
}

// buildNotifier is the default NotifierFactory.
func (s *Service) buildNotifier(ch *Channel) (Notifier, error) {
	switch ch.Type {
	case ChannelWebhook:
		if ch.Webhook != nil {
			return NewWebhookNotifier(*ch.Webhook, s.httpClient), nil
		}
	case ChannelTelegram:
		if ch.Telegram != nil {
			return NewTelegramNotifier(*ch.Telegram, s.telegramBaseURL, s.httpClient), nil
		}
	case ChannelEmail:
		if ch.Email != nil {
			return NewEmailNotifier(*ch.Email, s.tlsConfig), nil
		}
	case ChannelTwilio:
		if ch.Twilio != nil {
			return NewTwilioNotifier(*ch.Twilio, s.twilioBaseURL, s.httpClient), nil
		}
	}
	return nil, invalidf("channel %s has no %s configuration", ch.ID, ch.Type)
}

// newID returns prefix + 16 random hex characters.
func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// ListChannels returns all channels with secrets masked.
func (s *Service) ListChannels(ctx context.Context) ([]*Channel, error) {
	list, err := s.repo.ListChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	out := make([]*Channel, 0, len(list))
	for _, ch := range list {
		out = append(out, ch.Redacted())
	}
	return out, nil
}

// CreateChannel validates and stores a new channel, generating an ID when empty.
// Masked secrets are rejected with ErrMaskedSecret. The stored channel is returned
// with secrets masked.
func (s *Service) CreateChannel(ctx context.Context, ch *Channel) (*Channel, error) {
	if ch == nil {
		return nil, invalidf("channel is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	in := ch.Clone()
	if in.ID == "" {
		id, err := newID("ch_")
		if err != nil {
			return nil, err
		}
		in.ID = id
	} else if _, err := s.repo.GetChannel(ctx, in.ID); err == nil {
		return nil, fmt.Errorf("%w: channel %s", ErrAlreadyExists, in.ID)
	} else if !errors.Is(err, ErrChannelNotFound) {
		return nil, fmt.Errorf("lookup channel: %w", err)
	}

	if err := restoreSecrets(in, nil); err != nil {
		return nil, err
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	in.CreatedAt, in.UpdatedAt, in.LastDelivery = now, now, nil
	if err := s.repo.SaveChannel(ctx, in); err != nil {
		return nil, fmt.Errorf("save channel: %w", err)
	}
	return in.Redacted(), nil
}

// UpdateChannel replaces the configuration of the existing channel id. A masked secret
// ("******") keeps the stored value, provided the channel type is unchanged; otherwise
// ErrMaskedSecret is returned. Delivery history and creation time are preserved.
func (s *Service) UpdateChannel(ctx context.Context, id string, ch *Channel) (*Channel, error) {
	if ch == nil {
		return nil, invalidf("channel is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.repo.GetChannel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("lookup channel %s: %w", id, err)
	}
	in := ch.Clone()
	in.ID = existing.ID
	if err := restoreSecrets(in, existing); err != nil {
		return nil, err
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	in.CreatedAt = existing.CreatedAt
	in.LastDelivery = existing.LastDelivery
	in.UpdatedAt = time.Now().UTC()
	if err := s.repo.SaveChannel(ctx, in); err != nil {
		return nil, fmt.Errorf("save channel: %w", err)
	}
	return in.Redacted(), nil
}

// DeleteChannel removes a channel; rules referencing it keep their other channels
// (the ID is removed from them by the repository).
func (s *Service) DeleteChannel(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.DeleteChannel(ctx, id); err != nil {
		return fmt.Errorf("delete channel %s: %w", id, err)
	}
	return nil
}

// TestChannel synchronously sends a notification.test message to channel id (even if
// the channel is disabled) with a single attempt, records the outcome as the channel's
// last delivery and returns it. A delivery failure is returned as a non-nil error along
// with the recorded status.
func (s *Service) TestChannel(ctx context.Context, id string) (*DeliveryStatus, error) {
	ch, err := s.repo.GetChannel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("lookup channel %s: %w", id, err)
	}
	msg := Render(events.Event{Type: events.NotificationTest, Time: time.Now().UTC()})
	policy := s.retry
	policy.Retries = 0
	status, sendErr := s.send(ctx, ch, msg, policy)
	return status, sendErr
}

// ListRules returns all rules.
func (s *Service) ListRules(ctx context.Context) ([]*Rule, error) {
	list, err := s.repo.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	return list, nil
}

// CreateRule validates and stores a new rule, generating an ID when empty.
func (s *Service) CreateRule(ctx context.Context, r *Rule) (*Rule, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: rule is required", ErrInvalidRule)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	in := r.Clone()
	if in.ID == "" {
		id, err := newID("rule_")
		if err != nil {
			return nil, err
		}
		in.ID = id
	} else if _, err := s.repo.GetRule(ctx, in.ID); err == nil {
		return nil, fmt.Errorf("%w: rule %s", ErrAlreadyExists, in.ID)
	} else if !errors.Is(err, ErrRuleNotFound) {
		return nil, fmt.Errorf("lookup rule: %w", err)
	}
	if err := s.checkRuleLocked(ctx, in); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	in.CreatedAt, in.UpdatedAt = now, now
	if err := s.repo.SaveRule(ctx, in); err != nil {
		return nil, fmt.Errorf("save rule: %w", err)
	}
	return in.Clone(), nil
}

// UpdateRule replaces the existing rule id.
func (s *Service) UpdateRule(ctx context.Context, id string, r *Rule) (*Rule, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: rule is required", ErrInvalidRule)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.repo.GetRule(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("lookup rule %s: %w", id, err)
	}
	in := r.Clone()
	in.ID = existing.ID
	if err := s.checkRuleLocked(ctx, in); err != nil {
		return nil, err
	}
	in.CreatedAt = existing.CreatedAt
	in.UpdatedAt = time.Now().UTC()
	if err := s.repo.SaveRule(ctx, in); err != nil {
		return nil, fmt.Errorf("save rule: %w", err)
	}
	return in.Clone(), nil
}

// DeleteRule removes a rule.
func (s *Service) DeleteRule(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.DeleteRule(ctx, id); err != nil {
		return fmt.Errorf("delete rule %s: %w", id, err)
	}
	return nil
}

// checkRuleLocked validates r, de-duplicates its lists and verifies that every
// referenced channel exists. Caller must hold s.mu.
func (s *Service) checkRuleLocked(ctx context.Context, r *Rule) error {
	r.Events = dedupe(r.Events)
	r.JobIDs = dedupe(r.JobIDs)
	r.ChannelIDs = dedupe(r.ChannelIDs)
	if err := r.Validate(); err != nil {
		return err
	}
	for _, id := range r.ChannelIDs {
		if _, err := s.repo.GetChannel(ctx, id); err != nil {
			if errors.Is(err, ErrChannelNotFound) {
				return fmt.Errorf("%w: unknown channel %q", ErrInvalidRule, id)
			}
			return fmt.Errorf("lookup channel: %w", err)
		}
	}
	return nil
}

// dedupe removes duplicates while preserving order.
func dedupe[T comparable](in []T) []T {
	if in == nil {
		return nil
	}
	seen := make(map[T]struct{}, len(in))
	out := make([]T, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// HandleEvent is the events.Handler for the Bus: it matches e against enabled rules
// and enqueues one delivery per distinct enabled channel without blocking. Deliveries
// that do not fit in the queue are dropped and reported to the observer.
func (s *Service) HandleEvent(ctx context.Context, e events.Event) {
	if !e.Type.Subscribable() {
		return
	}
	rules, err := s.repo.ListRules(ctx)
	if err != nil {
		s.logger.Error("notification rules unavailable", slog.Any("error", err))
		return
	}

	var channelIDs []string
	seen := make(map[string]struct{})
	for _, r := range rules {
		if !r.Matches(e) {
			continue
		}
		for _, id := range r.ChannelIDs {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				channelIDs = append(channelIDs, id)
			}
		}
	}
	if len(channelIDs) == 0 {
		return
	}

	msg := Render(e)
	for _, id := range channelIDs {
		ch, err := s.repo.GetChannel(ctx, id)
		if err != nil {
			s.logger.Warn("notification channel unavailable", slog.String("channel_id", id), slog.Any("error", err))
			continue
		}
		if !ch.Enabled {
			continue
		}
		s.enqueue(delivery{channel: ch, msg: msg})
	}
}

// enqueue adds d to the queue without blocking.
func (s *Service) enqueue(d delivery) {
	if s.stopped.Load() {
		s.dropped(d, "service stopped")
		return
	}
	select {
	case s.queue <- d:
	default:
		s.dropped(d, "queue full")
	}
}

// dropped reports a delivery that was not queued.
func (s *Service) dropped(d delivery, reason string) {
	s.logger.Warn("notification dropped",
		slog.String("channel_id", d.channel.ID),
		slog.String("channel_type", string(d.channel.Type)),
		slog.String("event", string(d.msg.Event.Type)),
		slog.String("reason", reason),
	)
	s.observe(d.channel.Type, OutcomeDropped)
}

// observe forwards an outcome to the observer, if any.
func (s *Service) observe(t ChannelType, outcome string) {
	if s.observer != nil {
		s.observer(t, outcome)
	}
}

// Run starts the delivery worker pool and blocks until ctx is cancelled. Queued and
// in-flight deliveries then continue for at most the drain timeout before being
// aborted. Run may be called at most once.
func (s *Service) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrServiceRunning
	}

	// Deliveries run on a context detached from ctx so that shutdown does not abort
	// them immediately; it is cancelled once the drain timeout expires.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	var wg sync.WaitGroup
	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, workCtx)
		}()
	}

	<-ctx.Done()
	s.stopped.Store(true)
	timer := time.AfterFunc(s.drainTimeout, cancelWork)
	wg.Wait()
	timer.Stop()
	return nil
}

// worker sends queued deliveries until ctx ends, then drains the queue under workCtx.
func (s *Service) worker(ctx, workCtx context.Context) {
	for {
		select {
		case d := <-s.queue:
			s.process(workCtx, d)
		case <-ctx.Done():
			for {
				if workCtx.Err() != nil {
					return
				}
				select {
				case d := <-s.queue:
					s.process(workCtx, d)
				default:
					return
				}
			}
		}
	}
}

// process sends one queued delivery with the configured retry policy.
func (s *Service) process(ctx context.Context, d delivery) {
	_, _ = s.send(ctx, d.channel, d.msg, s.retry) // outcome is logged and recorded by send
}

// send delivers msg to ch with retries, records the outcome and reports it.
func (s *Service) send(ctx context.Context, ch *Channel, msg Message, policy RetryPolicy) (*DeliveryStatus, error) {
	status := DeliveryStatus{Event: msg.Event.Type}

	n, err := s.factory(ch)
	if err == nil {
		status.Attempts, err = SendWithRetry(ctx, n, msg, policy)
	}
	status.Time = time.Now().UTC()
	status.Success = err == nil
	if err != nil {
		err = scrub(err, secretsOf(ch)...)
		status.Error = truncate(singleLine(err.Error()), maxErrorLength)
		s.logger.Warn("notification delivery failed",
			slog.String("channel_id", ch.ID),
			slog.String("channel_type", string(ch.Type)),
			slog.String("event", string(msg.Event.Type)),
			slog.Int("attempts", status.Attempts),
			slog.String("error", status.Error),
		)
		s.observe(ch.Type, OutcomeFailure)
	} else {
		s.logger.Info("notification delivered",
			slog.String("channel_id", ch.ID),
			slog.String("channel_type", string(ch.Type)),
			slog.String("event", string(msg.Event.Type)),
			slog.Int("attempts", status.Attempts),
		)
		s.observe(ch.Type, OutcomeSuccess)
	}

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusPersistTimeout)
	defer cancel()
	if perr := s.repo.SaveDeliveryStatus(persistCtx, ch.ID, status); perr != nil && !errors.Is(perr, ErrChannelNotFound) {
		s.logger.Error("failed to persist notification delivery status",
			slog.String("channel_id", ch.ID),
			slog.Any("error", perr),
		)
	}
	return &status, err
}
