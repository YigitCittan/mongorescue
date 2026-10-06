package operations_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

var connErasures = []models.PostRestoreCommand{
	{Database: "*", Command: json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": 7}, "limit": 0}]}`)},
	{Database: "shop", Command: json.RawMessage(`{"drop": "sessions"}`)},
	{Database: "crm", Command: json.RawMessage(`{"drop": "leads"}`)},
}

func TestPreflightListsThePostRestoreCommands(t *testing.T) {
	env := newPreflightEnvWith(t, healthyInspector(), nil, connErasures)
	for _, dryRun := range []bool{false, true} {
		res, err := env.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, DryRun: dryRun})
		if err != nil {
			t.Fatal(err)
		}
		c := res.Check(models.PreflightCheckPostRestore)
		if c.Status != models.PreflightPass || !strings.Contains(c.Message, "delete on users in shop_rescue_") || !strings.Contains(c.Message, "drop on sessions") {
			t.Fatalf("dry run %v: post_restore check = %+v", dryRun, c)
		}
		if dryRun != strings.HasPrefix(c.Message, "dry run") {
			t.Fatalf("dry run %v: message %q", dryRun, c.Message)
		}
		// crm is not restored: two commands run, both in the clone.
		if len(res.PostRestore) != 2 {
			t.Fatalf("planned = %+v", res.PostRestore)
		}
		for _, p := range res.PostRestore {
			if p.Status != models.PostRestoreCommandPlanned || p.SourceDatabase != "shop" || !strings.HasPrefix(p.Database, "shop_rescue_") {
				t.Fatalf("planned = %+v", p)
			}
		}
	}

	// In place, nothing runs: the check warns.
	res, err := env.svc.PreflightRestore(admin(), inPlace(models.RestoreRequest{BackupID: env.backup.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Check(models.PreflightCheckPostRestore); c.Status != models.PreflightWarn || !strings.Contains(c.Message, "re-apply") || len(res.PostRestore) != 0 {
		t.Fatalf("in-place post_restore check = %+v, planned %+v", c, res.PostRestore)
	}

	// Without commands there is no check.
	plain := newPreflightEnv(t, healthyInspector(), nil)
	res, err = plain.svc.PreflightRestore(admin(), models.RestoreRequest{BackupID: plain.backup.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Checks {
		if c.ID == models.PreflightCheckPostRestore {
			t.Fatalf("unexpected post_restore check %+v", c)
		}
	}
}

func TestRestoresCarryTheConnectionsPostRestoreCommands(t *testing.T) {
	for _, verify := range []bool{false, true} {
		ins := healthyInspector()
		ins.manifest = backupManifest()
		env := newPreflightEnvWith(t, ins, nil, connErasures)
		rec, err := env.svc.StartRestore(admin(), models.RestoreRequest{BackupID: env.backup.ID, VerifyRestore: verify})
		if err != nil {
			t.Fatal(err)
		}
		done := waitRestoreDone(t, env.svc, rec.ID)
		if done.Status != models.RestoreStatusCompleted {
			t.Fatalf("restore = %+v", done)
		}
		env.engine.mu.Lock()
		req, posts := env.engine.requests[0], env.engine.postRestores
		env.engine.mu.Unlock()
		if len(req.PostRestoreCommands) != len(connErasures) {
			t.Fatalf("verify %v: Execute got %d commands; want the connection's %d", verify, len(req.PostRestoreCommands), len(connErasures))
		}
		// A verified restore defers them until the clone was compared with the backup.
		if req.DeferPostRestore != verify || (posts == 1) != verify {
			t.Fatalf("verify %v: deferred %v, RunPostRestore calls %d", verify, req.DeferPostRestore, posts)
		}
	}
}
