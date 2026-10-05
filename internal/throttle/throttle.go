// Package throttle limits the rate of a byte stream with a token bucket. The backup
// engine wraps the upload stream with it (max_upload_mbps), so a backup of a large
// database does not saturate the network link to its storage.
package throttle

import (
	"context"
	"io"
	"time"
)

// MinBurst is the smallest bucket: one read of up to this many bytes never waits
// before the rate is enforced.
const MinBurst = 64 << 10

// Clock is the time source of a Reader: Now reads the time and Sleep waits for d or
// until ctx ends, returning context.Cause(ctx) then.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// Option configures a Reader.
type Option func(*Reader)

// WithClock replaces the wall clock (tests).
func WithClock(c Clock) Option { return func(t *Reader) { t.clock = c } }

// WithBurst sets the bucket size in bytes (at least 1). The default is a tenth of
// a second of the rate, at least MinBurst.
func WithBurst(n int) Option { return func(t *Reader) { t.burst = max(n, 1) } }

// Reader is an io.Reader that passes at most bytesPerSecond on average from its
// source. Every Read reads at most one bucket and then waits until the bytes it
// returned are paid for, so over any span T at most rate*T plus one bucket pass.
// The wait ends early, with the context's cause, when ctx ends. A Reader is not
// safe for concurrent use (like most readers).
type Reader struct {
	ctx    context.Context
	r      io.Reader
	rate   float64 // bytes per second
	burst  int
	tokens float64
	last   time.Time
	clock  Clock
}

// NewReader returns r limited to bytesPerSecond, or r itself when the rate is not
// positive (unlimited).
func NewReader(ctx context.Context, r io.Reader, bytesPerSecond float64, opts ...Option) io.Reader {
	if bytesPerSecond <= 0 {
		return r
	}
	t := &Reader{ctx: ctx, r: r, rate: bytesPerSecond, clock: realClock{}}
	t.burst = max(int(bytesPerSecond/10), MinBurst)
	for _, opt := range opts {
		opt(t)
	}
	t.tokens = float64(t.burst)
	t.last = t.clock.Now()
	return t
}

// Read implements io.Reader.
func (t *Reader) Read(p []byte) (int, error) {
	if t.ctx.Err() != nil {
		return 0, context.Cause(t.ctx)
	}
	if len(p) > t.burst {
		p = p[:t.burst]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if waitErr := t.take(n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// take spends n tokens, refilling the bucket for the time since the last call,
// and sleeps off any debt.
func (t *Reader) take(n int) error {
	now := t.clock.Now()
	if elapsed := now.Sub(t.last); elapsed > 0 {
		t.tokens = min(float64(t.burst), t.tokens+elapsed.Seconds()*t.rate)
	}
	t.last = now
	t.tokens -= float64(n)
	if t.tokens >= 0 {
		return nil
	}
	return t.clock.Sleep(t.ctx, time.Duration(-t.tokens/t.rate*float64(time.Second)))
}
