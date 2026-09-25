package notify

import (
	"context"
	"fmt"
	"time"
)

// RetryPolicy controls delivery retries.
type RetryPolicy struct {
	// Retries is the number of additional attempts after the first one.
	Retries int
	// BaseBackoff is the delay before the first retry; it doubles on each retry.
	BaseBackoff time.Duration
	// MaxBackoff caps the delay between attempts.
	MaxBackoff time.Duration
	// AttemptTimeout bounds each individual attempt.
	AttemptTimeout time.Duration
}

// DefaultRetryPolicy is 3 retries with 1s/2s/4s backoff and a 10s per-attempt timeout.
var DefaultRetryPolicy = RetryPolicy{
	Retries:        3,
	BaseBackoff:    time.Second,
	MaxBackoff:     30 * time.Second,
	AttemptTimeout: SendTimeout,
}

// SendWithRetry calls n.Send until it succeeds, a permanent error is returned, the
// retry budget is exhausted, or ctx ends (backoff waits honour ctx). It returns the
// number of attempts made and the last error.
func SendWithRetry(ctx context.Context, n Notifier, msg Message, p RetryPolicy) (int, error) {
	if p.AttemptTimeout <= 0 {
		p.AttemptTimeout = SendTimeout
	}
	backoff := p.BaseBackoff
	attempts := 0
	for {
		attempts++
		attemptCtx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
		err := n.Send(attemptCtx, msg)
		cancel()
		if err == nil {
			return attempts, nil
		}
		if isPermanent(err) || attempts > p.Retries {
			return attempts, err
		}
		if ctx.Err() != nil {
			return attempts, fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempts, fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		case <-timer.C:
		}
		backoff *= 2
		if p.MaxBackoff > 0 && backoff > p.MaxBackoff {
			backoff = p.MaxBackoff
		}
	}
}
