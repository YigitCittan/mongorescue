package mcp

import (
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestStartBackupOfSeveralDatabases(t *testing.T) {
	f := newFixture(t, nil)
	cs := f.session(t, principal(auth.ScopeOperator))

	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{
		"connection_id": testConnID, "databases": []string{"shop", "crm"}, "parallelism": 2,
	}), &started)
	if started.RunID == "" || len(started.Backups) != 2 || started.Backup != nil || !strings.Contains(started.NextStep, "run_id") {
		t.Fatalf("start_backup with databases = %+v; want a run of two backups", started)
	}
	for i, want := range []string{"shop", "crm"} {
		b := started.Backups[i]
		if b.Database != want || b.RunID != started.RunID || b.Trigger != models.TriggerMCP {
			t.Errorf("backup %d = %+v", i, b)
		}
		if done := awaitBackup(t, cs, b.ID); done.Status != models.StatusCompleted {
			t.Errorf("backup of %s = %s", want, done.Status)
		}
	}

	for name, args := range map[string]map[string]any{
		"database and databases":   {"connection_id": testConnID, "database": "shop", "databases": []string{"crm"}},
		"collections with several": {"connection_id": testConnID, "databases": []string{"shop", "crm"}, "collections": []string{"orders"}},
		"duplicate":                {"connection_id": testConnID, "databases": []string{"shop", "shop"}},
	} {
		if res := call(t, cs, ToolStartBackup, args); !res.IsError {
			t.Errorf("%s: %+v; want a tool error", name, res)
		}
	}
}
