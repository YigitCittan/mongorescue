package app

import (
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// PostRestoreAuditAction is the audit log action of a post-restore command.
const PostRestoreAuditAction = "SYSTEM restore.post_restore_command"

// postRestoreAuditEvent is the audit log entry of one post-restore command of a
// restore: which command ran on which collection of which clone, and its counts.
// The command document and the documents it touched are never recorded.
func postRestoreAuditEvent(rec *models.RestoreRecord, res models.PostRestoreResult) auditlog.Event {
	outcome := auditlog.OutcomeOK
	if res.Status != models.PostRestoreCommandOK {
		outcome = auditlog.OutcomeError
	}
	targets := map[string]string{
		"restore_id":      rec.ID,
		"connection_id":   rec.TargetConnectionID,
		"database":        res.Database,
		"source_database": res.SourceDatabase,
		"collection":      res.Collection,
		"command":         res.Command,
		"index":           strconv.Itoa(res.Index),
		"n":               strconv.FormatInt(res.N, 10),
		"modified":        strconv.FormatInt(res.Modified, 10),
		"upserted":        strconv.FormatInt(res.Upserted, 10),
	}
	if res.Error != "" {
		targets["error"] = res.Error
	}
	return auditlog.Event{ActorKind: auditlog.ActorSystem, ActorName: "post_restore", Action: PostRestoreAuditAction,
		Targets: targets, Outcome: outcome}
}
