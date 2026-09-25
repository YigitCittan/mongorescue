package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Default Bus tuning values.
const (
	// DefaultBufferSize is the default capacity of the Bus queue.
	DefaultBufferSize = 256
	// DefaultDrainTimeout bounds how long Run keeps dispatching queued events after its
	// context is cancelled.
	DefaultDrainTimeout = 5 * time.Second
)

// ErrBusRunning is returned by Run when the Bus is already running or has already run.
var ErrBusRunning = errors.New("events: bus already started")

// Handler consumes an event. Handlers run sequentially on the Bus dispatcher goroutine
// and must therefore return quickly; slow work (network I/O) must be handed off to a
// worker pool owned by the subscriber.
type Handler func(ctx context.Context, e Event)

// Bus is an in-process, bounded, non-blocking event bus with a single dispatcher
// goroutine owned by Run. The zero value is not usable; construct it with NewBus.
type Bus struct {
	queue        chan Event
	logger       *slog.Logger
	drainTimeout time.Duration
	onDrop       func(Event)

	mu       sync.RWMutex
	handlers []Handler

	// stateMu makes "check stopped + enqueue" atomic with respect to stop, so no event
	// can be queued after the drain has begun (it would never be delivered).
	stateMu sync.RWMutex
	stopped bool

	started atomic.Bool
	dropped atomic.Uint64
}

// BusOption customises a Bus.
type BusOption func(*Bus)

// WithBufferSize sets the queue capacity. Non-positive values are ignored.
func WithBufferSize(n int) BusOption {
	return func(b *Bus) {
		if n > 0 {
			b.queue = make(chan Event, n)
		}
	}
}

// WithDrainTimeout sets how long Run keeps dispatching queued events after shutdown.
func WithDrainTimeout(d time.Duration) BusOption {
	return func(b *Bus) {
		if d > 0 {
			b.drainTimeout = d
		}
	}
}

// WithLogger sets the structured logger used for drop and panic reports.
func WithLogger(l *slog.Logger) BusOption {
	return func(b *Bus) {
		if l != nil {
			b.logger = l
		}
	}
}

// WithDropHook registers fn to be called (synchronously, on the publisher goroutine)
// every time an event is dropped. It must be cheap and non-blocking, e.g. a metric
// counter increment.
func WithDropHook(fn func(Event)) BusOption {
	return func(b *Bus) {
		b.onDrop = fn
	}
}

// NewBus constructs a Bus. Call Run to start dispatching.
func NewBus(opts ...BusOption) *Bus {
	b := &Bus{
		queue:        make(chan Event, DefaultBufferSize),
		logger:       slog.Default(),
		drainTimeout: DefaultDrainTimeout,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Subscribe registers h to receive every event dispatched after the call.
// A nil handler is ignored.
func (b *Bus) Subscribe(h Handler) {
	if h == nil {
		return
	}
	b.mu.Lock()
	b.handlers = append(b.handlers, h)
	b.mu.Unlock()
}

// Publish enqueues e without ever blocking. It returns false (and records a drop) when
// the queue is full or the Bus has stopped. The context is accepted for API symmetry
// with other I/O ports; a cancelled context does not prevent publication because the
// outcome being reported has already happened.
func (b *Bus) Publish(_ context.Context, e Event) bool {
	if b == nil {
		return false
	}
	b.stateMu.RLock()
	if b.stopped {
		b.stateMu.RUnlock()
		b.drop(e, "bus stopped", slog.LevelDebug)
		return false
	}
	select {
	case b.queue <- e:
		b.stateMu.RUnlock()
		return true
	default:
		b.stateMu.RUnlock()
		b.drop(e, "queue full", slog.LevelWarn)
		return false
	}
}

// stop marks the bus stopped; after it returns no Publish can enqueue.
func (b *Bus) stop() {
	b.stateMu.Lock()
	b.stopped = true
	b.stateMu.Unlock()
}

// Dropped returns the total number of events dropped since construction.
func (b *Bus) Dropped() uint64 {
	return b.dropped.Load()
}

// drop records a dropped event, logging it at level.
func (b *Bus) drop(e Event, reason string, level slog.Level) {
	b.dropped.Add(1)
	b.logger.Log(context.Background(), level, "event dropped",
		slog.String("event", string(e.Type)),
		slog.String("reason", reason),
	)
	if b.onDrop != nil {
		b.onDrop(e)
	}
}

// Run dispatches queued events to subscribers until ctx is cancelled. It then stops
// accepting new events and keeps dispatching already-queued events for at most the
// configured drain timeout, passing handlers a context bounded by that deadline.
// Run may be called at most once; subsequent calls return ErrBusRunning.
func (b *Bus) Run(ctx context.Context) error {
	if !b.started.CompareAndSwap(false, true) {
		return ErrBusRunning
	}

	for {
		select {
		case e := <-b.queue:
			if ctx.Err() != nil {
				// Shutdown raced with a ready event: hand it to the bounded drain.
				b.stop()
				b.drain(ctx, &e)
				return nil
			}
			b.dispatch(ctx, e)
		case <-ctx.Done():
			b.stop()
			b.drain(ctx, nil)
			return nil
		}
	}
}

// drain dispatches first (when non-nil) and every event still in the queue, bounded by
// the drain timeout.
func (b *Bus) drain(parent context.Context, first *Event) {
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), b.drainTimeout)
	defer cancel()

	if first != nil {
		b.dispatch(drainCtx, *first)
	}
	for {
		if drainCtx.Err() != nil {
			if left := len(b.queue); left > 0 {
				b.logger.Warn("event bus drain timed out", slog.Int("discarded", left))
			}
			return
		}
		select {
		case e := <-b.queue:
			b.dispatch(drainCtx, e)
		default:
			return
		}
	}
}

// dispatch invokes every handler with e, isolating handler panics.
func (b *Bus) dispatch(ctx context.Context, e Event) {
	b.mu.RLock()
	handlers := b.handlers
	b.mu.RUnlock()

	for _, h := range handlers {
		b.invoke(ctx, h, e)
	}
}

// invoke runs a single handler and recovers from a panic so one faulty subscriber
// cannot take down the dispatcher.
func (b *Bus) invoke(ctx context.Context, h Handler, e Event) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("event handler panicked",
				slog.String("event", string(e.Type)),
				slog.Any("panic", r),
			)
		}
	}()
	h(ctx, e)
}
