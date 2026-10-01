package mcp

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

type pinInput struct {
	BackupID string `json:"backup_id" jsonschema:"ID of a backup (see list_backups)"`
	Note     string `json:"note,omitempty" jsonschema:"why the backup is kept (shown in the dashboard)"`
}

type jobIDInput struct {
	JobID string `json:"job_id" jsonschema:"ID of the scheduled job (see list_jobs)"`
}

type verifyStarted struct {
	Backup   *models.BackupRecord `json:"backup"`
	NextStep string               `json:"next_step"`
}

// protective annotates a tool that records a check or protects data and never
// deletes or overwrites anything.
func protective(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: false, IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
}

// registerTrustTools registers verify_backup, pin_backup and retention_preview.
// Unpinning is deliberately not offered: it makes a backup deletable again.
func (s *Server) registerTrustTools() {
	addTool(s, &sdk.Tool{
		Name: ToolVerifyBackup,
		Description: "Re-read a completed backup's archive from its storage target and compare it with the checksum recorded " +
			"when it was written. Returns immediately; poll get_backup until verified_at changes, then read verification " +
			"(ok, mismatch or error).",
		Annotations: protective("Verify backup archive"),
		InputSchema: schemaFor[backupIDInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "backup_id") }),
	}, s.verifyBackup)
	addTool(s, &sdk.Tool{
		Name: ToolPinBackup,
		Description: "Pin a backup (legal hold): retention policies never delete it and it cannot be deleted until an " +
			"administrator unpins it in the dashboard.",
		Annotations: protective("Pin backup"),
		InputSchema: schemaFor[pinInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "backup_id")
			p["note"].MaxLength = ptr(operations.MaxPinNoteLength)
		}),
	}, s.pinBackup)
	addTool(s, &sdk.Tool{
		Name: ToolRetentionPreview,
		Description: "List the backups a job's retention policy would delete if it ran now, and why; pinned backups and " +
			"the newest verified backup are listed as protected. Nothing is deleted.",
		Annotations: readOnly("Preview retention"),
		InputSchema: schemaFor[jobIDInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "job_id") }),
	}, s.retentionPreview)
}

func (s *Server) verifyBackup(ctx context.Context, in backupIDInput) (verifyStarted, string, error) {
	if err := requireID("backup_id", in.BackupID); err != nil {
		return verifyStarted{}, "", err
	}
	rec, err := s.cfg.Operations.VerifyBackup(ctx, in.BackupID)
	if err != nil {
		return verifyStarted{}, "", err
	}
	next := fmt.Sprintf("poll get_backup with id %q until verified_at changes", rec.ID)
	return verifyStarted{Backup: rec, NextStep: next}, fmt.Sprintf("Verification of backup %s started; %s.", idText(rec.ID), next), nil
}

func (s *Server) pinBackup(ctx context.Context, in pinInput) (*models.BackupRecord, string, error) {
	if err := requireID("backup_id", in.BackupID); err != nil {
		return nil, "", err
	}
	rec, err := s.cfg.Operations.PinBackup(ctx, in.BackupID, in.Note)
	if err != nil {
		return nil, "", err
	}
	return rec, fmt.Sprintf("Backup %s is pinned.", idText(rec.ID)), nil
}

func (s *Server) retentionPreview(ctx context.Context, in jobIDInput) (*operations.RetentionPreview, string, error) {
	if err := requireID("job_id", in.JobID); err != nil {
		return nil, "", err
	}
	p, err := s.cfg.Operations.RetentionPreview(ctx, in.JobID, nil, nil)
	if err != nil {
		return nil, "", err
	}
	return p, fmt.Sprintf("Job %s: retention would delete %d backup(s) now; %d protected.", idText(p.JobID), len(p.Delete), len(p.Protected)), nil
}
