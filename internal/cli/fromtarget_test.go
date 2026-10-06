package cli

import (
	"context"
	"testing"
)

// TestRestoreFromTarget proves that --from-target names the storage target the
// restore reads from, in the preflight and in the restore.
func TestRestoreFromTarget(t *testing.T) {
	api := restoreAPI(preflightOK)
	code, stdout, stderr := runCLI(context.Background(), t, api, nil, "restore", "bkp_1", "--from-target", " stg_offsite ")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, route := range []string{"POST /restores/preflight", "POST /restore"} {
		got := api.sent(route)
		if len(got) != 1 || got[0].body["source_target_id"] != "stg_offsite" {
			t.Fatalf("%s: bodies = %v", route, got)
		}
	}
	if code, _, _ = runCLI(context.Background(), t, restoreAPI(preflightOK), nil, "restore", "--pitr", "conn_a",
		"--at", "2026-10-05T14:30:00Z", "--from-target", "stg_offsite"); code == ExitOK {
		t.Fatal("--from-target must not apply to --pitr")
	}
}
