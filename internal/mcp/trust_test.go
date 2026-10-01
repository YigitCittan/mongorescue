package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

func TestTrustTools(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := f.store.SaveBackupRecord(ctx, &models.BackupRecord{ID: "bkp_keep", JobID: testJobID, Trigger: models.TriggerScheduled,
		Database: "shop", Status: models.StatusCompleted, StartedAt: now.Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	op := f.session(t, principal(auth.ScopeOperator))
	var rec models.BackupRecord
	structured(t, ToolPinBackup, call(t, op, ToolPinBackup, map[string]any{"backup_id": "bkp_keep", "note": "audit"}), &rec)
	if !rec.Pinned || rec.PinNote != "audit" {
		t.Fatalf("pin_backup = %+v", rec)
	}
	if res := call(t, op, ToolVerifyBackup, map[string]any{"backup_id": "bkp_keep"}); !res.IsError || !strings.Contains(resultText(res), "not available") {
		t.Fatalf("verify_backup without the integrity service: %s", resultText(res))
	}

	read := f.session(t, principal(auth.ScopeRead))
	var preview operations.RetentionPreview
	structured(t, ToolRetentionPreview, call(t, read, ToolRetentionPreview, map[string]any{"job_id": testJobID}), &preview)
	if preview.JobID != testJobID || preview.Delete == nil {
		t.Fatalf("retention_preview = %+v", preview)
	}
	if res := call(t, read, ToolPinBackup, map[string]any{"backup_id": "bkp_keep"}); !res.IsError || !strings.Contains(resultText(res), `"operator" scope`) {
		t.Fatalf("a read key must not pin: %s", resultText(res))
	}
}
