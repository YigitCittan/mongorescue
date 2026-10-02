package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
)

// maxPollFailures is how many polls in a row may fail transiently (unreachable, 5xx,
// 429) before waiting gives up.
const maxPollFailures = 3

// maxRetryAfter caps the Retry-After delay of a rate-limited or unavailable poll.
const maxRetryAfter = 30 * time.Second

// waitTarget names what --wait waits for, for the progress lines and the message
// when waiting stops.
type waitTarget struct {
	// kind and id name the run ("backup", "bkp_...").
	kind, id string
	// hint is the command that shows the run later.
	hint string
}

// pollFunc checks a run once: done when it finished, and a line describing it.
type pollFunc func(ctx context.Context) (done bool, progress string, err error)

// pollInterval returns the interval between polls.
func (s *session) pollInterval() time.Duration {
	if s.app.PollInterval > 0 {
		return s.app.PollInterval
	}
	return DefaultPollInterval
}

// retryable reports whether a failed poll may be retried, and the least delay before
// the retry: the Retry-After of a 429 or 5xx answer, capped at maxRetryAfter.
func retryable(err error) (bool, time.Duration) {
	switch {
	case errors.Is(err, apiclient.ErrRateLimited), errors.Is(err, apiclient.ErrServer):
		var delay time.Duration
		if apiErr, ok := apiclient.AsAPIError(err); ok {
			delay = min(apiErr.RetryAfter, maxRetryAfter)
		}
		return true, delay
	case errors.Is(err, apiclient.ErrUnreachable):
		return true, 0
	}
	return false, 0
}

// wait polls until poll reports done. Progress lines go to stderr when they change
// (unless --quiet). Up to maxPollFailures polls in a row may fail with a 429, a 5xx
// or a network error; they are retried after the poll interval or the server's
// Retry-After (capped), whichever is longer. --timeout or cancelling ctx (Ctrl-C)
// stops only the waiting: it returns an ErrWaitStopped error naming the run, which
// keeps running on the server.
func (s *session) wait(ctx context.Context, target waitTarget, poll pollFunc) error {
	waitCtx := ctx
	if s.opts.timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, s.opts.timeout)
		defer cancel()
	}
	stopped := func() error {
		why := "interrupted"
		if ctx.Err() == nil {
			why = "timed out after " + s.opts.timeout.String()
		}
		return fmt.Errorf("%w for %s %s (%s); it keeps running on the server: %s",
			ErrWaitStopped, target.kind, target.id, why, target.hint)
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var last string
	failures := 0
	for {
		delay := s.pollInterval()
		done, progress, err := poll(waitCtx)
		if err != nil {
			if waitCtx.Err() != nil {
				return stopped()
			}
			retry, after := retryable(err)
			if !retry || failures >= maxPollFailures {
				return err
			}
			failures++
			delay = max(delay, after)
			s.warnf("polling %s %s failed (%d of %d), retrying in %s: %v", target.kind, target.id, failures, maxPollFailures, delay, err)
		} else {
			failures = 0
			if done {
				return nil
			}
			if progress != last {
				s.infof("%s %s: %s", target.kind, target.id, progress)
				last = progress
			}
		}
		timer.Reset(delay)
		select {
		case <-waitCtx.Done():
			return stopped()
		case <-timer.C:
		}
	}
}

// startError explains a request that starts a run (listed by "mongorescue list
// <list>") and was interrupted before its answer arrived: the server may or may not
// have started the run.
func startError(err error, list string) error {
	if errors.Is(err, context.Canceled) {
		return failedErrorf("cancelled before the request completed; the run may or may not have started, check `mongorescue list %s`", list)
	}
	return err
}
