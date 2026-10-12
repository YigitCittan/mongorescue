package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// TestPITRRestoreNeverInPlace checks that an MCP request cannot reach an
// in-place point-in-time restore (issue #142): the tool's input schema offers no
// in-place option, the request the tool builds is a restore into safe clones
// whatever its arguments hold, and a call with in-place arguments is not taken
// for one.
func TestPITRRestoreNeverInPlace(t *testing.T) {
	schema := schemaFor[pitrRestoreInput](nil)
	for _, name := range []string{"in_place", "confirm_in_place", "keep_clone", "pitr", "safe_clone", "target_database"} {
		if _, ok := schema.Properties[name]; ok {
			t.Fatalf("pitr_restore offers %q", name)
		}
	}
	raw := `{"stream_id":"str_a","at":"2026-10-05T12:00:00Z","databases":["shop"],"in_place":true,"confirm_in_place":"shop",` +
		`"keep_clone":true,"safe_clone":false,"pitr":{"stream_id":"str_a","in_place":true,"confirm_in_place":"shop"}}`
	var in pitrRestoreInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	req := pitrRestoreRequest(in, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if req.PITR.IsInPlace() || req.PITR.ConfirmInPlace != "" || req.PITR.KeepClone || req.InPlace() || req.ConfirmInPlace {
		t.Fatalf("pitr_restore built an in-place request: %+v (pitr %+v)", req, req.PITR)
	}
	if err := req.ValidatePITR(); err != nil {
		t.Fatalf("the safe-clone request is invalid: %v", err)
	}

	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeAdmin))
	res := call(t, cs, ToolPITRRestore, map[string]any{"stream_id": "str_a", "at": "2026-10-05T12:00:00Z", "databases": []string{"shop"},
		"in_place": true, "confirm_in_place": "shop", "keep_clone": true})
	if !res.IsError || strings.Contains(resultText(res), "in-place") || strings.Contains(resultText(res), "in_place\": true") {
		t.Fatalf("pitr_restore with in-place arguments: %v %s", res.IsError, resultText(res))
	}
}

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
