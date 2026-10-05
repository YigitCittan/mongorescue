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

	// An entry may be an object with a collection filter of its own.
	var filtered backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{
		"connection_id": testConnID,
		"databases":     []any{"crm", map[string]any{"name": "shop", "collections": []string{"orders"}}},
	}), &filtered)
	if len(filtered.Backups) != 2 || filtered.Backups[1].Database != "shop" ||
		len(filtered.Backups[1].Collections) != 1 || filtered.Backups[1].Collections[0] != "orders" || len(filtered.Backups[0].Collections) != 0 {
		t.Fatalf("start_backup with a filtered entry = %+v", filtered)
	}
	for _, b := range filtered.Backups {
		awaitBackup(t, cs, b.ID)
	}

	for name, args := range map[string]map[string]any{
		"entry without name":       {"connection_id": testConnID, "databases": []any{"crm", map[string]any{"collections": []string{"x"}}}},
		"entry with unknown field": {"connection_id": testConnID, "databases": []any{"crm", map[string]any{"name": "shop", "drop": true}}},
		"entry of another type":    {"connection_id": testConnID, "databases": []any{"crm", 3}},
		"database and databases":   {"connection_id": testConnID, "database": "shop", "databases": []string{"crm"}},
		"collections with several": {"connection_id": testConnID, "databases": []string{"shop", "crm"}, "collections": []string{"orders"}},
		"duplicate":                {"connection_id": testConnID, "databases": []string{"shop", "shop"}},
	} {
		if res := call(t, cs, ToolStartBackup, args); !res.IsError {
			t.Errorf("%s: %+v; want a tool error", name, res)
		}
	}
}
