package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
)

// TestSecurityAlertReachesEveryEnabledChannel checks that a broadcast event is sent
// to every enabled channel without any rule, and never to a disabled one.
func TestSecurityAlertReachesEveryEnabledChannel(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fakes := map[string]*fakeNotifier{"a": {}, "b": {}, "off": {}}
	rec := &outcomeRecorder{}
	svc := NewService(repo,
		WithNotifierFactory(func(ch *Channel) (Notifier, error) { return fakes[ch.ID], nil }),
		WithRetryPolicy(RetryPolicy{Retries: 0, BaseBackoff: time.Millisecond, AttemptTimeout: time.Second}),
		WithObserver(rec.observe),
	)
	for _, id := range []string{"a", "b", "off"} {
		ch := webhookChannel(id)
		ch.Enabled = id != "off"
		if _, err := svc.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()

	svc.HandleEvent(ctx, events.Event{Type: events.EncryptionOffAfterUpgrade, Time: time.Now()})

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fakes["a"].count() != 1 || fakes["b"].count() != 1 || fakes["off"].count() != 0 {
		t.Fatalf("sent a=%d b=%d off=%d; want 1, 1, 0", fakes["a"].count(), fakes["b"].count(), fakes["off"].count())
	}
	fakes["a"].mu.Lock()
	msg := fakes["a"].sent[0]
	fakes["a"].mu.Unlock()
	if !strings.Contains(msg.Subject, "Backup encryption is off") || !strings.Contains(msg.Body, "NOT encrypted") ||
		!strings.Contains(msg.Body, "Settings → Encryption") {
		t.Fatalf("message = %+v", msg)
	}
	if events.EncryptionOffAfterUpgrade.Subscribable() || !events.EncryptionOffAfterUpgrade.Broadcast() {
		t.Fatal("the security alert must be a broadcast, not a rule event")
	}
}
