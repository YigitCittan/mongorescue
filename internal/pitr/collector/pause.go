package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// pollInterval is how often PauseStream and ResumeStream look at the collector.
const pollInterval = 50 * time.Millisecond

// PauseStream stops the collector of stream id for a write MongoRescue makes
// itself that must not be replayed from this stream's oplog (the swap of an
// in-place point-in-time restore): it disables the stream, waits until its
// collector has stopped and ends its open chain with pitr.EndStopped at the
// collector's position, so the oplog written from now on never joins a chain that
// started before. wasEnabled reports whether the stream was enabled (then
// ResumeStream enables it again); a disabled stream's open chain is ended too. The
// wait is bounded by ctx. Expected failures: pitr.ErrNotFound.
func (s *Service) PauseStream(ctx context.Context, id string) (wasEnabled bool, err error) {
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return false, err
	}
	wasEnabled = st.Enabled
	if st.Enabled {
		st.Enabled = false
		if err = s.cfg.Repo.UpdateStream(ctx, st); err != nil {
			return false, fmt.Errorf("disable the stream: %w", err)
		}
		s.Reload()
	}
	if err = s.waitFor(ctx, func() (bool, error) { return !s.collecting(id), nil }); err != nil {
		return wasEnabled, fmt.Errorf("wait for the collector to stop: %w", err)
	}
	unlock, err := lockStream(ctx, id)
	if err != nil {
		return wasEnabled, err
	}
	defer unlock()
	chains, err := s.cfg.Repo.ListChains(ctx, id)
	if err != nil {
		return wasEnabled, fmt.Errorf("list the chains: %w", err)
	}
	for _, c := range chains {
		if !c.Open() {
			continue
		}
		if err := s.cfg.Repo.EndChain(ctx, id, c.ChainID, s.chainEnd(ctx, id), pitr.EndStopped, s.now()); err != nil && !errors.Is(err, pitr.ErrChainEnded) {
			return wasEnabled, fmt.Errorf("end the chain: %w", err)
		}
		s.logger.Info("PITR chain ended: the stream was paused", logsafe.Attr("stream_id", id), logsafe.Attr("chain_id", c.ChainID))
	}
	return wasEnabled, nil
}

// ResumeStream enables stream id again after PauseStream and waits, bounded by
// ctx, until its collector has opened a new chain (it starts one at the newest
// majority-committed entry when the stored position belongs to an ended chain),
// so a base backup started afterwards begins inside that chain. Expected
// failures: pitr.ErrNotFound, and ctx's error when no chain opened in time.
func (s *Service) ResumeStream(ctx context.Context, id string) error {
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return err
	}
	if !st.Enabled {
		st.Enabled = true
		if err = s.cfg.Repo.UpdateStream(ctx, st); err != nil {
			return fmt.Errorf("enable the stream: %w", err)
		}
	}
	s.Reload()
	err = s.waitFor(ctx, func() (bool, error) {
		chains, listErr := s.cfg.Repo.ListChains(ctx, id)
		if listErr != nil {
			return false, listErr
		}
		for _, c := range chains {
			if c.Open() {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("wait for the collector to open a new chain: %w", err)
	}
	return nil
}

// waitFor polls done until it reports true, fails, or ctx ends.
func (s *Service) waitFor(ctx context.Context, done func() (bool, error)) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
