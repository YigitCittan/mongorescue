package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestStoppingACollectorDoesNotBlockReaders(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	fx.svc.cfg.Clock = realClock{}
	fx.svc.cfg.ReconcileInterval = 10 * time.Millisecond
	// A collector stuck in a slow call that ignores cancellation until released;
	// its first call reports when the supervisor cancelled it. Later calls (after
	// a release on a failure) return at once.
	fx.svc.cfg.Open = func(wctx context.Context, _ *pitr.Stream) (Session, error) {
		enteredOnce.Do(func() {
			close(entered)
			go func() {
				<-wctx.Done()
				close(cancelled)
			}()
		})
		<-release
		return nil, context.Canceled
	}
	fx.svc.Start(ctx)
	// Cleanups run last in, first out: the hook is released before Stop waits
	// for the collector, whatever fails in between.
	t.Cleanup(fx.svc.Stop)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
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
	// The supervisor cancelled the collector, so it took it out of the running
	// set, and now waits for it (the hook ignores the cancellation).
	select {
	case <-cancelled:
	case <-time.After(time.Minute):
		t.Fatal("the supervisor did not stop the collector of the disabled stream")
	}

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
	case <-time.After(time.Minute):
		t.Fatal("Running and Status wait for a collector to stop")
	}
}
