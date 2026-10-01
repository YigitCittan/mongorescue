package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

func TestCancelRunTool(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	for _, b := range []*models.BackupRecord{
		{ID: "bkp_live", Database: "shop", ConnectionID: testConnID, Status: models.StatusInProgress, StartedAt: time.Now()},
		{ID: "bkp_done", Database: "shop", ConnectionID: testConnID, Status: models.StatusCompleted, StartedAt: time.Now()},
	} {
		if err := f.store.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.registry.Register(runs.Meta{Kind: models.RunBackup, ID: "bkp_live", Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx := run.Bind(ctx)
	defer run.End()

	if names := toolNames(t, f.session(t, principal(auth.ScopeRead))); slices.Contains(names, ToolCancelRun) {
		t.Fatal("read keys must not see cancel_run")
	}
	cs := f.session(t, principal(auth.ScopeOperator))
	var out runCancelled
	structured(t, ToolCancelRun, call(t, cs, ToolCancelRun, map[string]any{"id": "bkp_live"}), &out)
	if out.Kind != models.RunBackup || out.Backup == nil || out.Backup.ID != "bkp_live" || !strings.Contains(out.NextStep, "get_backup") {
		t.Fatalf("cancel_run = %+v", out)
	}
	<-runCtx.Done()
	if c := runs.CancellationOf(runCtx); c == nil || c.By != "API key operator key via MCP" {
		t.Fatalf("cancellation = %+v", c)
	}

	for id, want := range map[string]string{
		"bkp_done":    "not running",
		"bkp_nothing": "no backup or restore",
	} {
		res := call(t, cs, ToolCancelRun, map[string]any{"id": id})
		if !res.IsError || !strings.Contains(resultText(res), want) {
			t.Errorf("cancel_run %s = %v %q; want an error containing %q", id, res.IsError, resultText(res), want)
		}
	}
}
