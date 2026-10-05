package cli

import (
	"context"
	"strings"
	"testing"
)

func TestRestorePITRBody(t *testing.T) {
	api := restoreAPI(preflightOK)
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "restore", "--pitr", "str_a", "--at", "2026-10-05T14:30:00Z", "--database", "shop, crm")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, route := range []string{"POST /restores/preflight", "POST /restore"} {
		got := api.sent(route)
		if len(got) != 1 {
			t.Fatalf("%s sent %d times", route, len(got))
		}
		body := got[0].body
		p, _ := body["pitr"].(map[string]any)
		if p["stream_id"] != "str_a" || p["at"] != "2026-10-05T14:30:00Z" {
			t.Errorf("%s: pitr = %v", route, body["pitr"])
		}
		if dbs, _ := body["databases"].([]any); len(dbs) != 2 || dbs[0] != "shop" || dbs[1] != "crm" {
			t.Errorf("%s: databases = %v", route, body["databases"])
		}
		for _, k := range []string{"backup_id", "safe_clone", "confirm_in_place"} {
			if v, ok := body[k]; ok && v != "" {
				t.Errorf("%s: a point-in-time body has %s=%v", route, k, v)
			}
		}
	}
}

func TestRestorePITRGuard(t *testing.T) {
	for _, args := range [][]string{
		{"restore", "--pitr", "str_a"},
		{"restore", "--at", "2026-10-05T14:30:00Z"},
		{"restore", "--pitr", "str_a", "--at", "yesterday"},
		{"restore", "bkp_1", "--pitr", "str_a", "--at", "2026-10-05T14:30:00Z"},
		{"restore", "--pitr", "str_a", "--at", "2026-10-05T14:30:00Z", "--in-place", "--confirm"},
		{"restore", "--pitr", "str_a", "--at", "2026-10-05T14:30:00Z", "--collections", "orders"},
		{"restore", "--pitr", "str_a", "--at", "2026-10-05T14:30:00Z", "--dry-run"},
	} {
		api := restoreAPI(preflightOK)
		code, _, stderr := runCLI(context.Background(), t, api, nil, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d; want 2 (%s)", args, code, stderr)
		}
		api.mu.Lock()
		n := len(api.requests)
		api.mu.Unlock()
		if n != 0 {
			t.Errorf("%v: %d requests sent before the guard", args, n)
		}
		if !strings.Contains(stderr, "pitr") && !strings.Contains(stderr, "--at") {
			t.Errorf("%v: stderr %q does not explain", args, stderr)
		}
	}
}
