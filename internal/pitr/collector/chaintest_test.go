package collector

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChainTestSchedule(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	st, err := fx.repo.GetStream(ctx, fx.stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.Enabled, st.ChainTestCron = true, "@daily"
	if err = fx.repo.UpdateStream(ctx, st); err != nil {
		t.Fatal(err)
	}
	last := fx.clock.Now() // the stream was just created
	started, fail := 0, error(nil)
	fx.svc.cfg.NextRun = func(_ string, from time.Time) (time.Time, bool) { return from.Add(24 * time.Hour), true }
	fx.svc.cfg.LastChainTest = func(context.Context, string) time.Time { return last }
	fx.svc.cfg.StartChainTest = func(context.Context, string) error {
		if fail != nil {
			return fail
		}
		started++
		last = fx.clock.Now()
		return nil
	}

	fx.svc.runChainTests(ctx)
	if started != 0 {
		t.Fatal("a chain test started before the first activation")
	}
	fx.clock.advance(25 * time.Hour)
	fx.svc.runChainTests(ctx)
	fx.svc.runChainTests(ctx)
	if started != 1 {
		t.Fatalf("%d chain tests after one activation, want 1", started)
	}
	// A test that cannot start (no two bases) is not retried before the next activation.
	fail = errors.New("no two eligible bases")
	fx.clock.advance(25 * time.Hour)
	fx.svc.runChainTests(ctx)
	fail = nil
	fx.svc.runChainTests(ctx)
	if started != 1 {
		t.Fatalf("retried at once: %d", started)
	}
	fx.clock.advance(25 * time.Hour)
	fx.svc.runChainTests(ctx)
	if started != 2 {
		t.Fatalf("%d chain tests, want 2", started)
	}
	// Off without a schedule.
	st.ChainTestCron = ""
	if err = fx.repo.UpdateStream(ctx, st); err != nil {
		t.Fatal(err)
	}
	fx.clock.advance(25 * time.Hour)
	fx.svc.runChainTests(ctx)
	if started != 2 {
		t.Fatal("a chain test ran without a schedule")
	}
}
