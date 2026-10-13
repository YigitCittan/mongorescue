package operations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestKeyWrappingChangeIsAlerted(t *testing.T) {
	sink := &alertSink{}
	env := newProtEnv(t)
	env.cfg.Alerts = sink
	env.svc = operations.New(env.cfg)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	env.svc.KeyWrappingChanged(context.Background(), keywrap.Change{Action: keywrap.ActionRewrapped, At: at, Actor: "local:ops",
		Provider: keywrap.ProviderVaultTransit, KeyURI: "https://vault:8200/transit/keys/new",
		PreviousProvider: keywrap.ProviderAWSKMS, PreviousKeyURI: "alias/old", Fingerprint: "0123abcd", Files: []string{"secret.key"}})
	env.svc.KeyWrappingChanged(context.Background(), keywrap.Change{Action: keywrap.ActionWrapped, At: at})

	env.pub.mu.Lock()
	var got []events.Event
	for _, e := range env.pub.events {
		if e.Type == events.SecurityKeyWrapping {
			got = append(got, e)
		}
	}
	env.pub.mu.Unlock()
	if len(got) != 2 || len(sink.events()) != 2 {
		t.Fatalf("published %d, stored %d; want 2 each", len(got), len(sink.events()))
	}
	e := got[0]
	if e.Action != keywrap.ActionRewrapped || e.Actor != "local:ops" || !e.Time.Equal(at) ||
		!strings.Contains(e.Detail, "aws-kms:alias/old to vault-transit:https://vault:8200/transit/keys/new") || !strings.Contains(e.Detail, "0123abcd") {
		t.Fatalf("event = %+v", e)
	}
	if got[1].Actor != keywrap.ActorSystem {
		t.Errorf("actor of a change without one = %q", got[1].Actor)
	}
	if !events.SecurityKeyWrapping.Subscribable() {
		t.Error("security.key_wrapping_changed cannot be subscribed to")
	}
}
