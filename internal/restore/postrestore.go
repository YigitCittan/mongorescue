package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/postrestore"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// ErrPostRestoreFailed indicates that a post-restore command of the target
// connection failed or could not run: the restore failed and its clones were kept
// for inspection.
var ErrPostRestoreFailed = errors.New("restore: post-restore command failed")

// DefaultCommandTimeout bounds one post-restore command when RunConfig sets none.
const DefaultCommandTimeout = 60 * time.Second

// CommandSession runs the post-restore commands of one restore over one connection
// to the target (implemented by *mongoconn.CommandSession). Implementations must
// never include the URI's credentials, server messages or document contents in
// errors.
type CommandSession interface {
	// RunPostRestoreCommand runs command in database and returns its counts.
	RunPostRestoreCommand(ctx context.Context, database string, command json.RawMessage) (models.PostRestoreCounts, error)
	// Close releases the connection.
	Close()
}

// CommandRunner opens a CommandSession to the server at uri.
type CommandRunner func(ctx context.Context, uri string) (CommandSession, error)

// WithCommandRunner lets restores run the post-restore commands of their target
// connection (models.RestoreRequest.PostRestoreCommands), over one session per
// restore. Without it, a safe-clone restore with such commands fails instead of
// completing without them.
func WithCommandRunner(r CommandRunner) Option {
	return func(e *Engine) {
		e.commands = r
	}
}

// CommandAudit records one post-restore command that ran (successfully or not) in
// the audit log. result carries the command name and counts, never its document.
type CommandAudit func(ctx context.Context, record *models.RestoreRecord, result models.PostRestoreResult)

// WithCommandAudit records every post-restore command that ran through fn.
func WithCommandAudit(fn CommandAudit) Option {
	return func(e *Engine) {
		e.audit = fn
	}
}

// commandTimeout returns the limit of one post-restore command (RunConfig
// .CommandTimeout, DefaultCommandTimeout when unset).
func (e *Engine) commandTimeout() time.Duration {
	if t := e.runConfig().CommandTimeout; t > 0 {
		return t
	}
	return DefaultCommandTimeout
}

// keptNote is the end of the message of a restore whose post-restore commands
// failed.
func keptNote(clones []string) string {
	return fmt.Sprintf("; the restored database(s) %s were kept for inspection: they hold the restored data without every post-restore command applied (such as erasures), so do not use them, and drop them when done",
		strings.Join(clones, ", "))
}

// sortedClones returns the clone names of clones, sorted.
func sortedClones(clones map[string]string) []string {
	out := make([]string, 0, len(clones))
	for _, c := range clones {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// applyPostRestore runs the post-restore commands of req against the databases the
// restore created (clones: source database -> clone) and records them on record. A
// dry run and an in-place restore run none (the report says so, an in-place restore
// with a warning), nor does a request that defers them (DeferPostRestore). A failure
// returns an error wrapping ErrPostRestoreFailed; the caller fails the restore and
// keeps the clones.
func (e *Engine) applyPostRestore(ctx context.Context, uri string, req models.RestoreRequest, record *models.RestoreRecord, clones map[string]string) error {
	cmds := req.PostRestoreCommands
	switch {
	case len(cmds) == 0 || req.DeferPostRestore:
		return nil
	case req.DryRun:
		record.PostRestore = &models.PostRestoreReport{Status: models.PostRestoreSkipped,
			Note: fmt.Sprintf("dry run: nothing was restored, so none of the %d post-restore command(s) ran", len(cmds))}
		if steps, err := postrestore.Plan(cmds, clones); err == nil {
			record.PostRestore.Commands = postrestore.Planned(steps)
		}
		return nil
	case req.InPlace():
		note := fmt.Sprintf("the %d post-restore command(s) of the connection did not run: they run only against the safe clones a restore creates; re-apply them (such as erasures) to %s yourself",
			len(cmds), record.TargetDatabase)
		record.PostRestore = &models.PostRestoreReport{Status: models.PostRestoreSkipped, Note: note}
		addWarning(record, note)
		runs.FromContext(ctx).Printf("WARNING: %s", note)
		return nil
	}
	return e.runPostRestore(ctx, uri, cmds, record, clones)
}

// runPostRestore plans and runs cmds against clones, one after another, recording
// each on record and in the audit log. It stops at the first failure.
func (e *Engine) runPostRestore(ctx context.Context, uri string, cmds []models.PostRestoreCommand, record *models.RestoreRecord, clones map[string]string) error {
	tracker := runs.FromContext(ctx)
	report := &models.PostRestoreReport{Status: models.PostRestoreFailed}
	record.PostRestore = report
	steps, err := postrestore.Plan(cmds, clones)
	if err != nil {
		report.Note = redact.Text(err.Error())
		report.ClonesKept = sortedClones(clones)
		return fmt.Errorf("%w: %w", ErrPostRestoreFailed, err)
	}
	report.Commands = postrestore.Planned(steps)
	if len(steps) == 0 {
		report.Status = models.PostRestoreCompleted
		report.Note = fmt.Sprintf("none of the %d post-restore command(s) applies to the restored database(s)", len(cmds))
		return nil
	}
	if e.commands == nil {
		report.Note = "post-restore commands cannot run in this setup"
		report.ClonesKept = sortedClones(clones)
		return fmt.Errorf("%w: %s", ErrPostRestoreFailed, report.Note)
	}
	session, err := e.commands(ctx, uri)
	if err != nil {
		report.Note = "could not connect to the target: " + redact.Text(err.Error())
		report.ClonesKept = sortedClones(clones)
		return fmt.Errorf("%w: %s", ErrPostRestoreFailed, report.Note)
	}
	defer session.Close()
	timeout := e.commandTimeout()
	tracker.Printf("running %d post-restore command(s), each limited to %s: %s", len(steps), timeout, postrestore.Describe(steps))
	for i, step := range steps {
		start := time.Now()
		cmdCtx, cancel := context.WithTimeout(ctx, timeout)
		counts, runErr := session.RunPostRestoreCommand(cmdCtx, step.Target, step.Document)
		cancel()
		res := step.Result(models.PostRestoreCommandOK)
		res.PostRestoreCounts, res.DurationMS = counts, time.Since(start).Milliseconds()
		if runErr != nil {
			res.Status, res.PostRestoreCounts = models.PostRestoreCommandFailed, models.PostRestoreCounts{}
			res.Error = redact.Text(runErr.Error())
		}
		report.Commands[i] = res
		if e.audit != nil {
			e.audit(ctx, record, res)
		}
		if runErr != nil {
			for j := i + 1; j < len(steps); j++ {
				report.Commands[j].Status = models.PostRestoreCommandNotRun
			}
			report.ClonesKept = sortedClones(clones)
			report.Note = fmt.Sprintf("command %d of %d failed", i+1, len(steps))
			tracker.Printf("post-restore command %d of %d (%s on %s in %s) failed: %s", i+1, len(steps), step.Name, step.Collection, step.Target, res.Error)
			e.logger.Warn("post-restore command failed",
				slog.String("restore_id", record.ID), logsafe.Attr("target_db", step.Target),
				slog.String("command", step.Name), logsafe.Attr("collection", step.Collection), slog.String("error", res.Error))
			return fmt.Errorf("%w: command %d of %d (%s on %s in %s): %s", ErrPostRestoreFailed, i+1, len(steps), step.Name, step.Collection, step.Target, res.Error)
		}
		tracker.Printf("post-restore command %d of %d: %s on %s in %s: n=%d modified=%d upserted=%d (%d ms)",
			i+1, len(steps), step.Name, step.Collection, step.Target, res.N, res.Modified, res.Upserted, res.DurationMS)
	}
	report.Status = models.PostRestoreCompleted
	e.logger.Info("post-restore commands applied", slog.String("restore_id", record.ID), slog.Int("commands", len(steps)))
	return nil
}

// RunPostRestore runs the post-restore commands of req against the safe clone of a
// completed restore whose Execute deferred them (models.RestoreRequest
// .DeferPostRestore), once the caller has compared the clone with the backup. Other
// records (failed, in place, dry runs, point-in-time restores, none configured) are
// returned unchanged. A failed command fails the record (ErrPostRestoreFailed) and
// keeps the clone, which the record's message says.
func (e *Engine) RunPostRestore(ctx context.Context, req models.RestoreRequest, record *models.RestoreRecord) (*models.RestoreRecord, error) {
	if record == nil || record.Status != models.RestoreStatusCompleted || record.PITR != nil {
		return record, nil
	}
	req.DeferPostRestore = false
	// Bounded like the restore itself (RunConfig.Timeout), on top of the limit of
	// each command.
	if timeout := e.runConfig().Timeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, ErrTimeout)
		defer cancel()
	}
	clones := map[string]string{record.SourceDatabase: record.TargetDatabase}
	if err := e.applyPostRestore(ctx, e.resolveURI(req), req, record, clones); err != nil {
		record.Phases.Finished = models.Stamp(time.Now())
		return e.failDone(ctx, record, err, err.Error()+keptNote(sortedClones(clones)))
	}
	return record, nil
}
