package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestToolsFollowConnectionAccess calls every tool that reads or touches a
// connection with an operator key limited to testConnID: another connection, its
// job and its backup are as unknown as records that do not exist, and no answer
// names them.
func TestToolsFollowConnectionAccess(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RateLimit = RateLimit{PerMinute: 600, Burst: 100} })
	ctx := context.Background()
	now := time.Now().UTC()
	const (
		connB = "conn_team_b"
		jobB  = "job_team_b"
		bkpB  = "bkp_team_b"
		rstB  = "rst_team_b"
	)
	if err := f.store.SaveConnection(ctx, &models.Connection{ID: connB, Name: "Team B server", URI: "mongodb://b:27017", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveJob(ctx, &models.Job{ID: jobB, Name: "billing nightly", Database: "billing", ConnectionID: connB, CronExpression: "@daily", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: bkpB, JobID: jobB, Database: "billing", ConnectionID: connB,
		Status: models.StatusCompleted, StartedAt: now, StorageKey: "billing/x.archive"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveRestoreRecord(ctx, &models.RestoreRecord{ID: rstB, BackupID: bkpB, SourceDatabase: "billing", TargetDatabase: "billing_rescue",
		SourceConnectionID: connB, TargetConnectionID: connB, Status: models.RestoreStatusCompleted, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	p := principal(auth.ScopeOperator)
	p.Connections = auth.OnlyConnections(testConnID)
	cs := f.session(t, p)
	markers := []string{connB, jobB, bkpB, rstB, "billing", "Team B"}
	leak := func(out *sdk.CallToolResult) string {
		b, _ := json.Marshal(out)
		for _, m := range markers {
			if strings.Contains(string(b), m) {
				return m
			}
		}
		return ""
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{ToolListConnections, nil},
		{ToolListJobs, nil},
		{ToolListBackups, nil},
		{ToolListRestores, nil},
		{ToolListStorageTargets, nil},
		{ToolGetStatus, nil},
	} {
		out := call(t, cs, tc.tool, tc.args)
		if out.IsError {
			t.Errorf("%s: %s", tc.tool, resultText(out))
		}
		if m := leak(out); m != "" {
			t.Errorf("%s names %q of another connection: %s", tc.tool, m, resultText(out))
		}
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{ToolListDatabases, map[string]any{"connection_id": connB}},
		{ToolListCollections, map[string]any{"connection_id": connB, "database": "billing"}},
		{ToolGetJob, map[string]any{"id": jobB}},
		{ToolGetBackup, map[string]any{"id": bkpB}},
		{ToolGetRestore, map[string]any{"id": rstB}},
		{ToolListBackupCollections, map[string]any{"backup_id": bkpB}},
		{ToolStartBackup, map[string]any{"connection_id": connB, "database": "billing"}},
		{ToolRunJob, map[string]any{"job_id": jobB}},
		{ToolRestoreSafeClone, map[string]any{"backup_id": bkpB}},
		{ToolCancelRun, map[string]any{"id": bkpB}},
		{ToolVerifyBackup, map[string]any{"backup_id": bkpB}},
		{ToolPinBackup, map[string]any{"backup_id": bkpB}},
		{ToolRetentionPreview, map[string]any{"job_id": jobB}},
		{ToolPreviewJobDatabases, map[string]any{"job_id": jobB}},
		{ToolListJobRuns, map[string]any{"job_id": jobB}},
	} {
		out := call(t, cs, tc.tool, tc.args)
		if !out.IsError || !strings.Contains(strings.ToLower(resultText(out)), "not found") && !strings.Contains(resultText(out), "no backup or restore") {
			t.Errorf("%s on another connection = %s; want not found", tc.tool, resultText(out))
		}
	}
	// The key's own connection works.
	if out := call(t, cs, ToolGetJob, map[string]any{"id": testJobID}); out.IsError {
		t.Errorf("get_job of the key's connection: %s", resultText(out))
	}
}
