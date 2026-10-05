package collector

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestStoppingACollectorDoesNotBlockReaders(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	fx.svc.cfg.Clock = realClock{}
	fx.svc.cfg.ReconcileInterval = 10 * time.Millisecond
	// A collector stuck in a slow call that ignores cancellation for a while.
	fx.svc.cfg.Open = func(context.Context, *pitr.Stream) (Session, error) {
		close(entered)
		<-release
		return nil, context.Canceled
	}
	fx.svc.Start(ctx)
	defer fx.svc.Stop()
	<-entered

	st, err := fx.repo.GetStream(ctx, fx.stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.Enabled = false
	if err = fx.repo.UpdateStream(ctx, st); err != nil {
		t.Fatal(err)
	}
	fx.svc.Reload()
	time.Sleep(50 * time.Millisecond) // the supervisor now waits for the collector

	done := make(chan bool, 1)
	go func() {
		running := fx.svc.Running(fx.stream.ID)
		_, statusErr := fx.svc.Status(ctx, fx.stream.ID)
		done <- running || statusErr != nil
	}()
	select {
	case bad := <-done:
		if bad {
			t.Fatal("the stopping collector is still reported running, or Status failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Running and Status wait for a collector to stop")
	}
	close(release)
}
