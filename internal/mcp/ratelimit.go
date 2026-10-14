package mcp

import (
	"time"

	"github.com/yigitcittan/mongorescue/internal/ratelimit"
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

// limiter keeps one token bucket per API key (internal/ratelimit, which evicts
// idle buckets).
type limiter struct {
	buckets *ratelimit.Limiter
	limit   ratelimit.Limit
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
		buckets: ratelimit.New(ratelimit.WithClock(now)),
		limit:   ratelimit.Limit{PerMinute: rl.PerMinute, Burst: rl.Burst},
	}
}

// allow consumes one token of key's bucket. When the bucket is empty it returns false
// and the time until the next token.
func (l *limiter) allow(key string) (bool, time.Duration) {
	return l.buckets.Allow(key, l.limit)
}
