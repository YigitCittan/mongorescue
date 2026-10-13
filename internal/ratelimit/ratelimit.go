// Package ratelimit keeps one token bucket per key (an API key, a session, a client
// address) in memory, for the request limits of the REST API and the MCP endpoint.
// Memory stays bounded: buckets that have refilled completely are as good as new and
// are evicted, and a hard cap on the number of buckets evicts arbitrary ones.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limit is a budget: PerMinute requests sustained, Burst at once. A Limit with
// PerMinute 0 (or less) is off: every request is allowed.
type Limit struct {
	// PerMinute is the sustained number of requests per minute.
	PerMinute int `json:"per_minute"`
	// Burst is the number of requests allowed at once (at least 1).
	Burst int `json:"burst"`
}

// Enabled reports whether l limits anything.
func (l Limit) Enabled() bool { return l.PerMinute > 0 }

// every returns the refill rate of l.
func (l Limit) every() rate.Limit { return rate.Limit(float64(l.PerMinute) / 60) }

// size returns the bucket size of l (at least 1).
func (l Limit) size() int { return max(l.Burst, 1) }

// Bookkeeping defaults.
const (
	// DefaultMaxKeys bounds the buckets a Limiter keeps.
	DefaultMaxKeys = 100_000
	// sweepSize is the number of buckets from which idle ones are swept.
	sweepSize = 1024
)

// Limiter keeps a token bucket per key. It is safe for concurrent use.
type Limiter struct {
	now     func() time.Time
	maxKeys int

	mu        sync.Mutex
	buckets   map[string]*bucket
	nextSweep int
}

// bucket is one key's token bucket and the limit it was made for.
type bucket struct {
	lim   *rate.Limiter
	limit Limit
}

// Option customises a Limiter.
type Option func(*Limiter)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(l *Limiter) { l.now = now } }

// WithMaxKeys bounds the buckets kept at once (DefaultMaxKeys by default). When it
// is reached after a sweep, an arbitrary bucket is evicted for each new key: its
// key starts with a full bucket again.
func WithMaxKeys(n int) Option {
	return func(l *Limiter) {
		if n > 0 {
			l.maxKeys = n
		}
	}
}

// New returns an empty Limiter.
func New(opts ...Option) *Limiter {
	l := &Limiter{now: time.Now, maxKeys: DefaultMaxKeys, buckets: map[string]*bucket{}, nextSweep: sweepSize}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Allow takes one token from the bucket of key under limit. When the bucket is
// empty it takes nothing and returns false and the time until the next token. A
// disabled limit allows everything and keeps no bucket. A changed limit applies to
// the key's existing bucket from now on.
func (l *Limiter) Allow(key string, limit Limit) (ok bool, wait time.Duration) {
	if !limit.Enabled() {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, found := l.buckets[key]
	switch {
	case !found:
		l.makeRoom(now)
		b = &bucket{lim: rate.NewLimiter(limit.every(), limit.size()), limit: limit}
		l.buckets[key] = b
	case b.limit != limit:
		b.lim.SetLimitAt(now, limit.every())
		b.lim.SetBurstAt(now, limit.size())
		b.limit = limit
	}
	r := b.lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Minute
	}
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// makeRoom evicts the buckets that refilled completely once the map reached
// nextSweep, and arbitrary buckets while it holds maxKeys. Caller holds mu.
func (l *Limiter) makeRoom(now time.Time) {
	if len(l.buckets) >= l.nextSweep {
		for k, b := range l.buckets {
			if b.lim.TokensAt(now) >= float64(b.limit.size()) {
				delete(l.buckets, k)
			}
		}
		l.nextSweep = max(sweepSize, 2*len(l.buckets))
	}
	for k := range l.buckets {
		if len(l.buckets) < l.maxKeys {
			break
		}
		delete(l.buckets, k)
	}
}

// Len returns the number of buckets kept.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Gate lets an event through at most once per interval per key (such as a log
// line per rate-limited caller). Keys that passed longer than interval ago are
// forgotten, so its memory stays bounded by the keys of one interval. It is safe
// for concurrent use.
type Gate struct {
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	last      map[string]time.Time
	nextSweep int
}

// NewGate returns a Gate for interval; now is the time source (time.Now when nil).
func NewGate(interval time.Duration, now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{interval: interval, now: now, last: map[string]time.Time{}, nextSweep: sweepSize}
}

// Pass reports whether key may pass now: true the first time and then once more
// every interval.
func (g *Gate) Pass(key string) bool {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if at, ok := g.last[key]; ok && now.Sub(at) < g.interval {
		return false
	}
	if len(g.last) >= g.nextSweep {
		for k, at := range g.last {
			if now.Sub(at) >= g.interval {
				delete(g.last, k)
			}
		}
		g.nextSweep = max(sweepSize, 2*len(g.last))
	}
	for k := range g.last {
		if len(g.last) < DefaultMaxKeys {
			break
		}
		delete(g.last, k)
	}
	g.last[key] = now
	return true
}
