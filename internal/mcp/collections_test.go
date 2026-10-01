package mcp

import (
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// TestListBackupCollectionsAndSelectiveRestore lists the collections of a backup with
// a read key and restores one of them with an operator key. The fixture's fake dumps
// are not real archives, so the list falls back to the backup's collection filter.
func TestListBackupCollectionsAndSelectiveRestore(t *testing.T) {
	f := newFixture(t, nil)
	op := f.session(t, principal(auth.ScopeOperator))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, op, ToolStartBackup, map[string]any{
		"connection_id": testConnID, "database": "shop", "collections": []string{"orders", "customers"},
	}), &started)
	b := awaitBackup(t, op, started.Backup.ID)

	read := f.session(t, principal(auth.ScopeRead))
	res := call(t, read, ToolListBackupCollections, map[string]any{"backup_id": b.ID})
	var list operations.BackupCollections
	structured(t, ToolListBackupCollections, res, &list)
	assertNoSecret(t, ToolListBackupCollections, res)
	if list.Source != operations.CollectionsFromRecord || len(list.Collections) != 2 || list.Warning == "" ||
		!strings.Contains(resultText(res), "could not be read") {
		t.Fatalf("list_backup_collections = %+v (%s)", list, resultText(res))
	}

	if res := call(t, read, ToolListBackupCollections, map[string]any{"backup_id": "bkp_missing"}); !res.IsError || !strings.Contains(resultText(res), "not found") {
		t.Fatalf("missing backup = %s; want a not-found tool error", resultText(res))
	}

	var rst restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, op, ToolRestoreSafeClone, map[string]any{"backup_id": b.ID, "collections": []string{"orders"}}), &rst)
	done := awaitRestore(t, op, rst.Restore.ID)
	if !slices.Equal(done.SelectedCollections, []string{"orders"}) || !strings.Contains(done.TargetDatabase, "_rescue_") {
		t.Fatalf("restore = %+v; want a safe clone of orders only", done)
	}
}
