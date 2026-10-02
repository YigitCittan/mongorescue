package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
)

// maxPollFailures is how many polls in a row may fail transiently (unreachable, 5xx)
// before waiting gives up.
const maxPollFailures = 3

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

// wait polls until poll reports done. Progress lines go to stderr when they change
// (unless --quiet). --timeout or cancelling ctx (Ctrl-C) stops only the waiting: it
// returns an ErrWaitStopped error naming the run, which keeps running on the server.
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
	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()
	var last string
	failures := 0
	for {
		done, progress, err := poll(waitCtx)
		switch {
		case err != nil && waitCtx.Err() != nil:
			return stopped()
		case err != nil && (errors.Is(err, apiclient.ErrUnreachable) || errors.Is(err, apiclient.ErrServer)) && failures < maxPollFailures:
			failures++
			s.warnf("polling %s %s failed (%d of %d): %v", target.kind, target.id, failures, maxPollFailures, err)
		case err != nil:
			return err
		default:
			failures = 0
			if done {
				return nil
			}
			if progress != last {
				s.infof("%s %s: %s", target.kind, target.id, progress)
				last = progress
			}
		}
		select {
		case <-waitCtx.Done():
			return stopped()
		case <-ticker.C:
		}
	}
}
