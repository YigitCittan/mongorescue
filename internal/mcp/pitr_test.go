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
	op := f.session(t, principal(auth.ScopeOperator))
	if res := call(t, op, ToolPITRRestore, args); !res.IsError {
		t.Fatalf("an operator key started a point-in-time restore: %s", resultText(res))
	}
	adm := f.session(t, principal(auth.ScopeAdmin))
	if res := call(t, adm, ToolPITRRestore, map[string]any{"stream_id": "str_a", "at": "yesterday"}); !res.IsError || !strings.Contains(resultText(res), "RFC 3339") {
		t.Fatalf("bad time: %v %s", res.IsError, resultText(res))
	}
	if res := call(t, adm, ToolPITRRestore, args); !res.IsError || !strings.Contains(resultText(res), "not available") {
		t.Fatalf("without PITR restores: %v %s", res.IsError, resultText(res))
	}
}
