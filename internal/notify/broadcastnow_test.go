package notify

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// TestBroadcastNowSendsWithoutTheWorkers checks the direct delivery of an alert
// that must leave before the notification workers stop: no worker runs, every
// enabled channel receives it once, a disabled one does not.
func TestBroadcastNowSendsWithoutTheWorkers(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fakes := map[string]*fakeNotifier{"a": {}, "b": {}, "off": {}}
	svc := NewService(repo,
		WithNotifierFactory(func(ch *Channel) (Notifier, error) { return fakes[ch.ID], nil }),
		WithRetryPolicy(RetryPolicy{Retries: 3, BaseBackoff: time.Millisecond, AttemptTimeout: time.Second}),
	)
	for _, id := range []string{"a", "b", "off"} {
		ch := webhookChannel(id)
		ch.Enabled = id != "off"
		if _, err := svc.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	sent := svc.BroadcastNow(ctx, events.Event{Type: events.HASteppedDown, Time: time.Now(), Detail: "instance a stepped down"})
	if sent != 2 || fakes["a"].count() != 1 || fakes["b"].count() != 1 || fakes["off"].count() != 0 {
		t.Fatalf("sent %d (a=%d b=%d off=%d); want 2, 1, 1, 0", sent, fakes["a"].count(), fakes["b"].count(), fakes["off"].count())
	}
}
