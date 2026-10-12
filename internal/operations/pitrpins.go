package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// pitrPinAttempts bounds how often the start of a point-in-time restore plans
// again when part of its plan was deleted before it was pinned.
const pitrPinAttempts = 3

// pitrPins returns the pins of the restores in flight (Config.PITRPins, else
// pitr.SharedPins, the set PITR retention and the purges honour).
func (s *Service) pitrPins() *pitr.PinSet {
	if s.cfg.PITRPins != nil {
		return s.cfg.PITRPins
	}
	return pitr.SharedPins()
}

// pinPITRPlan pins the base backup and the oplog chunks pp's run reads (by ID,
// which covers a copy chain too) for the whole restore, so retention and the
// purges skip them; runPITRRestore releases the pin (pp.unpin) on every exit
// path. It pins first and checks second: what retention deletes after the pin
// keeps its object until the release, and what it deleted before fails the
// check. Then replan plans again, up to pitrPinAttempts times in all; a plan
// that keeps changing fails with ErrPITRNotRestorable instead of starting a
// restore bound to fail.
func (s *Service) pinPITRPlan(ctx context.Context, pp *pitrPlan, replan func() (*pitrPlan, error)) (*pitrPlan, error) {
	for attempt := 1; ; attempt++ {
		err := s.pinAndCheck(ctx, pp)
		switch {
		case err == nil:
			return pp, nil
		case !errors.Is(err, pitr.ErrPlanStale):
			return nil, err
		case attempt >= pitrPinAttempts:
			return nil, public("retention deleted part of the point-in-time restore plan while it was being planned ("+
				strings.TrimPrefix(err.Error(), "pitr: ")+"); try again", ErrPITRNotRestorable, err)
		}
		s.logger.Info("PITR retention deleted part of a restore plan before it was pinned; planning again",
			logsafe.Attr("stream_id", pp.stream.ID), logsafe.Error(err))
		if pp, err = replan(); err != nil {
			return nil, err
		}
	}
}

// pinAndCheck pins the run of pp and checks that it still exists (see
// pinPITRPlan); on success pp.unpin releases the pin, on failure it is released.
func (s *Service) pinAndCheck(ctx context.Context, pp *pitrPlan) error {
	run := pp.run
	if run.Plan == nil || run.Base == nil {
		return nil
	}
	chunks := make([]string, len(run.Plan.Chunks))
	for i, c := range run.Plan.Chunks {
		chunks[i] = c.ID
	}
	owner := "point-in-time restore of stream " + pp.stream.ID
	if pp.chainTest {
		owner = "chain test of stream " + pp.stream.ID
	}
	release := s.pitrPins().Pin(owner, []string{run.Base.ID}, chunks)
	if err := s.checkPinnedPlan(ctx, pp); err != nil {
		release()
		return err
	}
	pp.unpin = release
	return nil
}

// checkPinnedPlan returns pitr.ErrPlanStale when a chunk of pp's plan or its base
// backup was deleted since it was planned.
func (s *Service) checkPinnedPlan(ctx context.Context, pp *pitrPlan) error {
	if err := pitr.CheckPlanLive(ctx, s.cfg.PITR, pp.run.Plan); err != nil {
		return err
	}
	records, err := s.cfg.PITRBases(ctx, pp.stream.ID)
	if err != nil {
		return fmt.Errorf("list base backups: %w", err)
	}
	if _, byID := pitrBases(records); byID[pp.run.Base.ID] == nil {
		return fmt.Errorf("%w: base backup %s was deleted", pitr.ErrPlanStale, pp.run.Base.ID)
	}
	return nil
}
