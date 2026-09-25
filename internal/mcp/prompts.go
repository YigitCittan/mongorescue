package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Prompt names.
const (
	PromptDiagnoseFailedBackup = "diagnose_failed_backup"
	PromptDisasterRecoveryPlan = "disaster_recovery_plan"
	PromptVerifyRecentBackups  = "verify_recent_backups"
)

// maxPromptHours bounds verify_recent_backups.
const maxPromptHours = 24 * 90

func (s *Server) registerPrompts() {
	s.sdk.AddPrompt(&sdk.Prompt{
		Name:        PromptDiagnoseFailedBackup,
		Title:       "Diagnose a failed backup",
		Description: "Find out why a backup failed and what to do about it, using the read-only tools.",
		Arguments:   []*sdk.PromptArgument{{Name: "backup_id", Description: "ID of the failed backup (see list_backups with status failed)", Required: true}},
	}, promptHandler(func(args map[string]string) (string, error) {
		id, err := promptArg(args, "backup_id")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`Diagnose why the MongoRescue backup %q failed.

1. Call get_backup with id %q. Read status, error_message, database, connection_id, storage_target_id and started_at.
2. If it belongs to a job (job_id), call get_job to see its schedule and settings, and list_backups for the same database to see whether earlier runs succeeded and when the failures started.
3. Check the source: call list_databases for the backup's connection_id to see whether the server is reachable and the database still exists.
4. Check the destination: call list_storage_targets and look at the backup's storage target (last_test_ok).
5. Classify the cause (authentication, network, missing database, storage, timeout or stall, encryption key, other) and explain it in plain words.
6. Recommend concrete next steps. If a retry makes sense, you may offer start_backup (or run_job for a job), but ask the user first.

Do not try to delete anything or change settings; those actions are not available through MCP.`, id, id), nil
	}))

	s.sdk.AddPrompt(&sdk.Prompt{
		Name:        PromptDisasterRecoveryPlan,
		Title:       "Disaster recovery plan",
		Description: "Plan (and, with approval, rehearse) the recovery of a database from its latest good backup into a safe clone.",
		Arguments:   []*sdk.PromptArgument{{Name: "database", Description: "the database to recover", Required: true}},
	}, promptHandler(func(args map[string]string) (string, error) {
		db, err := promptArg(args, "database")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`Prepare a disaster recovery plan for the MongoDB database %q with MongoRescue.

1. Call list_backups with database %q and status "completed" to find the newest good backups. Note id, started_at, size_bytes, sha256, encrypted and storage_target_id.
2. Call get_status to check that the service is healthy and whether backups of this database failed recently.
3. Propose a recovery point (usually the newest completed backup) and state the expected data loss window (time since that backup started).
4. With the user's approval, rehearse the recovery: call restore_to_safe_clone with the chosen backup_id and verify true. It restores into a NEW database named %s_rescue_<timestamp>, so no existing data is touched.
5. Poll get_restore until the status is completed or failed, then report the clone's database name and whether the archive was verified.
6. Explain how to validate the clone (document counts, spot checks) and that switching applications over, or an in-place restore, is done by a person in the MongoRescue dashboard; it is intentionally not available through MCP.`, db, db, db), nil
	}))

	s.sdk.AddPrompt(&sdk.Prompt{
		Name:        PromptVerifyRecentBackups,
		Title:       "Verify recent backups",
		Description: "Check that every scheduled job produced a successful backup recently and report the gaps.",
		Arguments:   []*sdk.PromptArgument{{Name: "hours", Description: "look-back window in hours (default 24)"}},
	}, promptHandler(func(args map[string]string) (string, error) {
		hours := 24
		if v := strings.TrimSpace(args["hours"]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxPromptHours {
				return "", fmt.Errorf("%w: hours must be a whole number between 1 and %d", errInvalidInput, maxPromptHours)
			}
			hours = n
		}
		return fmt.Sprintf(`Verify the MongoRescue backups of the last %d hours.

1. Call get_status. For every entry of job_status, check that last_success_at is within the last %d hours; list enabled jobs without a recent success as gaps.
2. Review failed_last_24h and, if the window is longer, call list_backups with status "failed" to cover the whole window.
3. For each database with a recent completed backup, note its size and compare it with earlier backups (list_backups for that database); flag sudden drops in size.
4. Summarise: healthy jobs, gaps, failures with their error messages, and suspicious sizes.
5. Suggest remedies. You may offer run_job for a job with a gap, or a restore_to_safe_clone rehearsal with verify true for a spot check, but ask the user before starting anything.`, hours, hours), nil
	}))
}

// promptArg returns a required prompt argument.
func promptArg(args map[string]string, name string) (string, error) {
	v := strings.TrimSpace(args[name])
	if v == "" {
		return "", fmt.Errorf("%w: %s is required", errInvalidInput, name)
	}
	if len(v) > maxIDLength {
		return "", fmt.Errorf("%w: %s is too long", errInvalidInput, name)
	}
	return v, nil
}

// promptHandler adapts a text template to a prompt handler returning one user message.
func promptHandler(render func(args map[string]string) (string, error)) sdk.PromptHandler {
	return func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		text, err := render(req.Params.Arguments)
		if err != nil {
			return nil, err
		}
		return &sdk.GetPromptResult{
			Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: text}}},
		}, nil
	}
}
