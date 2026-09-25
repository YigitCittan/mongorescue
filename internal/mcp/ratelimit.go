package mcp

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimit bounds MCP calls per API key with a token bucket.
type RateLimit struct {
	// PerMinute is the sustained number of calls per minute.
	PerMinute int
	// Burst is the number of calls allowed at once.
	Burst int
}

// DefaultRateLimit allows 60 calls per minute with bursts of 20, per API key.
var DefaultRateLimit = RateLimit{PerMinute: 60, Burst: 20}

// Limiter bookkeeping.
const (
	// limiterSweepSize triggers a sweep of idle buckets.
	limiterSweepSize = 1024
	// limiterIdle is how long an unused bucket is kept.
	limiterIdle = 10 * time.Minute
)

// limiter keeps one token bucket per API key.
type limiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is one key's token bucket and its last use.
type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// newLimiter returns a limiter for rl (DefaultRateLimit for the zero value).
func newLimiter(rl RateLimit, now func() time.Time) *limiter {
	if rl.PerMinute <= 0 {
		rl.PerMinute = DefaultRateLimit.PerMinute
	}
	if rl.Burst <= 0 {
		rl.Burst = DefaultRateLimit.Burst
	}
	return &limiter{
		limit:   rate.Limit(float64(rl.PerMinute) / 60),
		burst:   rl.Burst,
		now:     now,
		buckets: map[string]*bucket{},
	}
}

// allow consumes one token of key's bucket. When the bucket is empty it returns false
// and the time until the next token.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) >= limiterSweepSize {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > limiterIdle {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	r := b.lim.ReserveN(now, 1)
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now)
		return false, delay
	}
	return true, 0
}
