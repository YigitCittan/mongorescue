package app

import (
	"context"
	"strconv"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// commandRunner opens the post-restore command sessions of restores through p.
func commandRunner(p *mongoconn.Prober) restore.CommandRunner {
	return func(ctx context.Context, uri string) (restore.CommandSession, error) {
		s, err := p.OpenCommandSession(ctx, uri)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

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
