package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestPostRestoreAuditEventRecordsNameAndCountsOnly(t *testing.T) {
	rec := &models.RestoreRecord{ID: "rst_shop_1", TargetConnectionID: "conn_1"}
	res := models.PostRestoreResult{Index: 2, Database: "shop_rescue_x", SourceDatabase: "shop", Command: "update", Collection: "users",
		Status: models.PostRestoreCommandOK, PostRestoreCounts: models.PostRestoreCounts{N: 3, Modified: 2, Upserted: 1}}
	e := postRestoreAuditEvent(rec, res)
	if e.ActorKind != auditlog.ActorSystem || e.Action != PostRestoreAuditAction || e.Outcome != auditlog.OutcomeOK {
		t.Fatalf("event = %+v", e)
	}
	want := map[string]string{"restore_id": "rst_shop_1", "connection_id": "conn_1", "database": "shop_rescue_x", "source_database": "shop",
		"collection": "users", "command": "update", "index": "2", "n": "3", "modified": "2", "upserted": "1"}
	for k, v := range want {
		if e.Targets[k] != v {
			t.Errorf("targets[%s] = %q; want %q", k, e.Targets[k], v)
		}
	}
	if len(e.Targets) != len(want) {
		t.Fatalf("targets = %v; want exactly %v", e.Targets, want)
	}

	res.Status, res.Error = models.PostRestoreCommandFailed, "mongoconn: post-restore command failed: 1 write error(s), first code 11000"
	e = postRestoreAuditEvent(rec, res)
	if e.Outcome != auditlog.OutcomeError || !strings.Contains(e.Targets["error"], "11000") {
		t.Fatalf("failed event = %+v", e)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "$set") || strings.Contains(string(raw), "deletes") {
		t.Fatalf("the event must not hold the command document: %s", raw)
	}
}
