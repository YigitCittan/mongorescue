package server

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/mcp"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// toolRoutes maps every MCP tool to the REST route that performs the same operation.
var toolRoutes = map[string]string{
	mcp.ToolListConnections:       "GET /api/v1/connections",
	mcp.ToolListDatabases:         "GET /api/v1/connections/{id}/databases",
	mcp.ToolListCollections:       "GET /api/v1/connections/{id}/databases/{db}/collections",
	mcp.ToolListJobs:              "GET /api/v1/jobs",
	mcp.ToolGetJob:                "GET /api/v1/jobs/{id}",
	mcp.ToolListBackups:           "GET /api/v1/backups",
	mcp.ToolGetBackup:             "GET /api/v1/backups",
	mcp.ToolListRestores:          "GET /api/v1/restores",
	mcp.ToolGetRestore:            "GET /api/v1/restores",
	mcp.ToolListStorageTargets:    "GET /api/v1/storage-targets",
	mcp.ToolGetStatus:             "GET /api/v1/stats",
	mcp.ToolStartBackup:           "POST /api/v1/backups",
	mcp.ToolRunJob:                "POST /api/v1/jobs/{id}/run",
	mcp.ToolRestoreSafeClone:      "POST /api/v1/restore",
	mcp.ToolListBackupCollections: "GET /api/v1/backups/{id}/collections",
	mcp.ToolCancelRun:             "POST /api/v1/backups/{id}/cancel",
	mcp.ToolVerifyBackup:          "POST /api/v1/backups/{id}/verify",
	mcp.ToolPinBackup:             "POST /api/v1/backups/{id}/pin",
	mcp.ToolRetentionPreview:      "GET /api/v1/jobs/{id}/retention/preview",
	mcp.ToolPreviewJobDatabases:   "GET /api/v1/jobs/{id}/databases/preview",
	mcp.ToolListJobRuns:           "GET /api/v1/jobs/{id}/runs",
}

// TestMCPToolsNeedTheScopeOfTheirRESTRoute checks that no tool is easier to reach
// than the REST route doing the same thing: each tool requires exactly the scope of
// its route in routeScopes. A new tool without an entry in toolRoutes fails here.
func TestMCPToolsNeedTheScopeOfTheirRESTRoute(t *testing.T) {
	for tool, scope := range mcp.ToolScopes {
		route, ok := toolRoutes[tool]
		if !ok {
			t.Errorf("tool %s has no REST equivalent in toolRoutes; add one", tool)
			continue
		}
		want, ok := routeScopes[route]
		if !ok {
			t.Errorf("tool %s maps to %q, which is not in routeScopes", tool, route)
			continue
		}
		if scope != want {
			t.Errorf("tool %s needs %q; its REST route %s needs %q", tool, scope, route, want)
		}
	}
	for tool := range toolRoutes {
		if _, ok := mcp.ToolScopes[tool]; !ok {
			t.Errorf("toolRoutes has a stale entry %s", tool)
		}
	}
}

// TestMCPPreviewFitsTheWriteTimeout checks that list_backup_collections gives up on a
// slow archive early enough for its record fallback to be written before the HTTP
// server's write timeout cuts the MCP response (REST extends its own deadline).
func TestMCPPreviewFitsTheWriteTimeout(t *testing.T) {
	if margin := writeTimeout - mcp.PreviewTimeout; margin < 5*time.Second {
		t.Fatalf("MCP preview timeout %s leaves %s before the %s write timeout; want at least 5s", mcp.PreviewTimeout, margin, writeTimeout)
	}
	if mcp.PreviewTimeout >= operations.ArchivePreviewTimeout {
		t.Fatalf("MCP preview timeout %s must be shorter than the default %s", mcp.PreviewTimeout, operations.ArchivePreviewTimeout)
	}
}
