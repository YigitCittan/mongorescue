package notify

import (
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/events"
)

func TestRenderKeyWrappingChanged(t *testing.T) {
	msg := Render(events.Event{Type: events.SecurityKeyWrapping, Action: "unwrapped_to_file", Actor: "local:ops",
		Detail: "secret.key was unwrapped from aws-kms:alias/k and written back as a plaintext file"})
	if !strings.Contains(msg.Subject, "Secret key wrapping changed") || !strings.Contains(msg.Subject, "unwrapped_to_file by local:ops") {
		t.Fatalf("subject = %q", msg.Subject)
	}
}
