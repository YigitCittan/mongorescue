package mcp

import (
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

func TestPITRTools(t *testing.T) {
	f := newFixture(t, nil)
	read := f.session(t, principal(auth.ScopeRead))
	var out pitrStatusOutput
	structured(t, ToolPITRStatus, call(t, read, ToolPITRStatus, nil), &out)
	if len(out.Streams) != 0 {
		t.Fatalf("streams = %v without PITR", out.Streams)
	}

	args := map[string]any{"stream_id": "str_a", "at": "2026-10-05T12:00:00Z", "databases": []string{"shop"}}
	if res := call(t, read, ToolPITRRestore, args); !res.IsError {
		t.Fatalf("a read key started a point-in-time restore: %s", resultText(res))
	}
	// Operator keys may restore into safe clones (design decision 8), like admin
	// keys: both get past the scope check to the request's own refusals.
	for _, scope := range []auth.Scope{auth.ScopeOperator, auth.ScopeAdmin} {
		cs := f.session(t, principal(scope))
		if res := call(t, cs, ToolPITRRestore, map[string]any{"stream_id": "str_a", "at": "yesterday"}); !res.IsError || !strings.Contains(resultText(res), "RFC 3339") {
			t.Fatalf("%s bad time: %v %s", scope, res.IsError, resultText(res))
		}
		if res := call(t, cs, ToolPITRRestore, args); !res.IsError || !strings.Contains(resultText(res), "not available") {
			t.Fatalf("%s without PITR restores: %v %s", scope, res.IsError, resultText(res))
		}
	}
}
