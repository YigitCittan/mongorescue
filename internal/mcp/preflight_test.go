package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// tinyTarget is a reachable restore target whose server reports almost no free disk.
type tinyTarget struct{}

func (tinyTarget) OpenTarget(context.Context, string) (connections.RestoreTarget, error) {
	return tinyTarget{}, nil
}

func (tinyTarget) Manifest(context.Context, string, string) (*models.Manifest, error) {
	return &models.Manifest{}, nil
}

func (tinyTarget) Ping(context.Context) (connections.ServerInfo, error) {
	return connections.ServerInfo{Version: "7.0.14"}, nil
}

func (tinyTarget) DatabaseExists(context.Context, string) (bool, error) { return false, nil }

func (tinyTarget) ListCollections(context.Context, string) ([]connections.Collection, error) {
	return nil, nil
}

func (tinyTarget) Privileges(context.Context, string, []string, []string) (connections.PrivilegeReport, error) {
	return connections.PrivilegeReport{Certain: true}, nil
}

func (tinyTarget) FreeSpace(context.Context, string) (connections.DiskSpace, error) {
	return connections.DiskSpace{Known: true, Free: 1, Source: connections.DiskSpaceDBStats}, nil
}

func (tinyTarget) Close() {}

func TestRestoreToolReportsThePreflightAndTakesForce(t *testing.T) {
	f := newFixtureWith(t, nil, tinyTarget{})
	cs := f.session(t, principal(auth.ScopeOperator))
	var started backupStarted
	structured(t, ToolStartBackup, call(t, cs, ToolStartBackup, map[string]any{"connection_id": testConnID, "database": "shop"}), &started)
	done := awaitBackup(t, cs, started.Backup.ID)

	// The 1-byte target refuses the restore with the failed check.
	res := call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": done.ID})
	if !res.IsError || !strings.Contains(resultText(res), "disk_space") || !strings.Contains(resultText(res), "force") {
		t.Fatalf("restore_to_safe_clone on a full target = %+v; want the failed disk_space check", res)
	}

	var rst restoreStarted
	structured(t, ToolRestoreSafeClone, call(t, cs, ToolRestoreSafeClone, map[string]any{"backup_id": done.ID, "force": true, "verify_restore": true}), &rst)
	if rst.Restore == nil || !rst.Restore.Forced || rst.Preflight == nil || rst.Preflight.OK ||
		rst.Preflight.Check(models.PreflightCheckDiskSpace).Status != models.PreflightFail {
		t.Fatalf("forced restore_to_safe_clone = %+v", rst)
	}
	final := awaitRestore(t, cs, rst.Restore.ID)
	if final.Status != models.RestoreStatusCompleted || final.Verification == nil {
		t.Fatalf("forced restore = %+v; want completed with a verification", final)
	}
}
