package auditlog

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/audit"
)

// MCP methods that only read; they stay in the activity log alone.
const (
	mcpReadResource = "resources/read"
	mcpGetPrompt    = "prompts/get"
)

// FromActivity maps an entry of the API key activity log (internal/audit) to an
// audit log entry, for the calls this log does not record itself: MCP tool calls
// (action "MCP <tool>") and system actions ("SYSTEM <tool>"). REST entries, resource
// reads and prompt requests are not mirrored (false). The arguments are not copied:
// only top-level string arguments named id or ending in _id become targets.
func FromActivity(e audit.Entry) (Event, bool) {
	out := Event{Time: e.Time, Outcome: activityOutcome(e.Result), Count: e.Count, Targets: targetsOf(e.Arguments)}
	switch e.Transport {
	case audit.TransportHTTP, audit.TransportStdio:
		if e.Tool == mcpReadResource || e.Tool == mcpGetPrompt {
			return Event{}, false
		}
		out.ActorKind, out.ActorKeyID, out.ActorKeyName = ActorAPIKey, e.APIKeyID, e.APIKeyName
		out.Action = "MCP " + e.Tool
	case audit.TransportSystem:
		out.ActorKind, out.ActorName = ActorSystem, e.APIKeyName
		out.Action = "SYSTEM " + e.Tool
	default:
		return Event{}, false
	}
	return out, true
}

// Mirror returns an observer for audit.WithObserver that records the activity
// entries FromActivity maps into s.
func (s *Service) Mirror() func(context.Context, audit.Entry) {
	return func(ctx context.Context, e audit.Entry) {
		if ev, ok := FromActivity(e); ok {
			s.Record(ctx, ev)
		}
	}
}

// activityOutcome maps an activity result to an outcome.
func activityOutcome(result string) string {
	switch result {
	case audit.ResultOK:
		return OutcomeOK
	case audit.ResultDenied:
		return OutcomeDenied
	case audit.ResultRateLimited:
		return OutcomeRateLimited
	default:
		return OutcomeError
	}
}

// targetsOf returns the top-level string arguments named id or *_id.
func targetsOf(raw json.RawMessage) map[string]string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range args {
		if str, ok := v.(string); ok && (k == "id" || strings.HasSuffix(k, "_id")) {
			out[k] = str
		}
	}
	return out
}
