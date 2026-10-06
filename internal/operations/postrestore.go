package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/postrestore"
)

// ConnectionEditor changes connections (implemented by *connections.Service, which
// the Connections of Config usually is).
type ConnectionEditor interface {
	// Update replaces the editable fields of connection id.
	Update(ctx context.Context, id string, in connections.Input) (*models.Connection, error)
	// SetPostRestoreCommands replaces the post-restore commands of connection id.
	SetPostRestoreCommands(ctx context.Context, id string, cmds []models.PostRestoreCommand) (*models.Connection, error)
}

// Audit log targets of a connection update.
const (
	auditPostRestoreChanged = "post_restore_commands_changed"
	auditPostRestoreFrom    = "post_restore_commands_from"
	auditPostRestoreTo      = "post_restore_commands_to"
)

// editor returns the connection editor, or ErrUnavailable.
func (s *Service) editor() (ConnectionEditor, error) {
	ed, ok := s.cfg.Connections.(ConnectionEditor)
	if !ok {
		return nil, public("connections cannot be changed in this setup", ErrUnavailable)
	}
	return ed, nil
}

// UpdateConnection updates connection id like connections.Service.Update and
// records in the audit log whether its post-restore commands changed, with their
// counts before and after (never their contents). The post-restore commands are an
// erasure log: while the two-person rule is on, a change that removes or changes
// any of them (and so could bring erased data back after a restore) waits for a
// second administrator (models.ApprovalPostRestoreCommands, returned as an
// *ApprovalPendingError); the rest of the update is saved at once. Adding commands
// needs no approval.
func (s *Service) UpdateConnection(ctx context.Context, id string, in connections.Input) (*models.Connection, error) {
	ed, err := s.editor()
	if err != nil {
		return nil, err
	}
	if in.PostRestoreCommands == nil {
		auditlog.Annotate(ctx, auditPostRestoreChanged, "false")
		return ed.Update(ctx, id, in)
	}
	cur, err := s.cfg.Connections.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *in.PostRestoreCommands
	changed := !sameCommands(cur.PostRestoreCommands, next)
	auditlog.Annotate(ctx, auditPostRestoreChanged, strconv.FormatBool(changed))
	if changed {
		auditlog.Annotate(ctx, auditPostRestoreFrom, strconv.Itoa(len(cur.PostRestoreCommands)))
		auditlog.Annotate(ctx, auditPostRestoreTo, strconv.Itoa(len(next)))
	}
	removed := removedCommands(cur.PostRestoreCommands, next)
	if removed == 0 || !s.needsApproval(ctx) {
		return ed.Update(ctx, id, in)
	}
	// The held list must be valid now, so the approval cannot fail on it later.
	if err = postrestore.Validate(next); err != nil {
		return nil, fmt.Errorf("%w: post_restore_commands: %w", connections.ErrInvalid, err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("encode post-restore commands: %w", err)
	}
	rest := in
	rest.PostRestoreCommands = nil
	if _, err = ed.Update(ctx, id, rest); err != nil {
		return nil, err
	}
	created := cur.CreatedAt
	return nil, s.requestApproval(ctx, &models.Approval{
		Action: models.ApprovalPostRestoreCommands, Subject: id, SubjectCreatedAt: &created, Secret: string(raw),
		Summary: fmt.Sprintf("change the post-restore commands of connection %s from %d to %d command(s), removing or changing %d of them (erasures they re-apply after restores would no longer run)",
			cur.Name, len(cur.PostRestoreCommands), len(next), removed),
	})
}

// applyPostRestoreChange runs an approved models.ApprovalPostRestoreCommands.
func (s *Service) applyPostRestoreChange(ctx context.Context, a *models.Approval) (string, error) {
	ed, err := s.editor()
	if err != nil {
		return "", err
	}
	if a.Secret == "" {
		return "", public("the request holds no post-restore commands", ErrInvalid)
	}
	var cmds []models.PostRestoreCommand
	if err = json.Unmarshal([]byte(a.Secret), &cmds); err != nil {
		return "", public("the request's post-restore commands cannot be read", ErrInvalid)
	}
	cur, err := s.cfg.Connections.Resolve(ctx, a.Subject)
	if err != nil {
		return "", err
	}
	if a.SubjectCreatedAt != nil && !cur.CreatedAt.Equal(*a.SubjectCreatedAt) {
		return "", public("connection "+a.Subject+" was replaced since the request", ErrInvalid)
	}
	if _, err = ed.SetPostRestoreCommands(ctx, a.Subject, cmds); err != nil {
		return "", err
	}
	auditlog.Annotate(ctx, auditPostRestoreChanged, "true")
	auditlog.Annotate(ctx, auditPostRestoreFrom, strconv.Itoa(len(cur.PostRestoreCommands)))
	auditlog.Annotate(ctx, auditPostRestoreTo, strconv.Itoa(len(cmds)))
	return fmt.Sprintf("post-restore commands of connection %s set to %d command(s)", cur.Name, len(cmds)), nil
}

// DatabaseDropper drops a database on a server (implemented by *mongoconn.Prober).
type DatabaseDropper interface {
	// DropDatabase drops database on the server at uri.
	DropDatabase(ctx context.Context, uri, database string) error
}

// KeptClones returns the restores the caller in ctx may see whose post-restore
// commands failed and whose kept clones were not dropped yet: those clones still
// hold data the commands should have erased.
func (s *Service) KeptClones(ctx context.Context) ([]*models.RestoreRecord, error) {
	recs, err := s.store.ListRestoreRecords(ctx)
	if err != nil {
		return nil, err
	}
	var out []*models.RestoreRecord
	for _, r := range recs {
		if r.PostRestore.KeepsClones() {
			out = append(out, r)
		}
	}
	return out, nil
}

// DropKeptClones drops the clones restore id kept after a failed post-restore
// command (admin), by their recorded names only: each must be a clone of that
// restore (its target database, or one of its point-in-time clones) with a
// "_rescue_" name, and never its source database. The record notes when and by
// whom; the action is audited and published as a destructive action. Expected
// failures: ErrNotFound, ErrInvalid (no kept clones, already dropped, a name that
// is not one of the restore's clones), ErrUnavailable and auth.ErrForbidden.
func (s *Service) DropKeptClones(ctx context.Context, id string) (*models.RestoreRecord, error) {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return nil, fmt.Errorf("dropping kept clones needs the admin role or an admin API key: %w", err)
	}
	if s.cfg.Dropper == nil {
		return nil, public("databases cannot be dropped in this setup", ErrUnavailable)
	}
	rec, err := s.store.GetRestoreRecord(ctx, id)
	if err != nil {
		return nil, notFound(err, "restore not found")
	}
	pr := rec.PostRestore
	switch {
	case pr == nil || len(pr.ClonesKept) == 0:
		return nil, public("restore "+id+" kept no clones after its post-restore commands", ErrInvalid)
	case pr.ClonesDroppedAt != nil:
		return nil, public("the clones of restore "+id+" were dropped already", ErrInvalid)
	}
	for _, name := range pr.ClonesKept {
		if !keptCloneOf(rec, name) {
			return nil, public(fmt.Sprintf("refusing to drop %s: it is not a clone restore %s created", name, id), ErrInvalid)
		}
	}
	conn, err := s.ResolveConnection(ctx, rec.TargetConnectionID)
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, name := range pr.ClonesKept {
		if dropErr := s.cfg.Dropper.DropDatabase(mongotls.NewContext(ctx, conn.TLS()), conn.URI, name); dropErr != nil {
			s.logger.Warn("failed to drop a clone kept after a failed post-restore command",
				logsafe.Attr("restore_id", id), logsafe.Attr("database", name), logsafe.Error(dropErr))
			failed = append(failed, name)
		}
	}
	auditlog.Annotate(ctx, "databases", strings.Join(pr.ClonesKept, ","))
	if len(failed) > 0 {
		return nil, fmt.Errorf("drop the kept clones %s: %s could not be dropped; retry or drop them by hand",
			strings.Join(pr.ClonesKept, ", "), strings.Join(failed, ", "))
	}
	now := s.now().UTC()
	pr.ClonesDroppedAt, pr.ClonesDroppedBy = &now, principalName(ctx)
	if err = s.store.SaveRestoreRecord(ctx, rec); err != nil {
		return nil, fmt.Errorf("save restore record: %w", err)
	}
	s.destructive(ctx, "drop_kept_clones", fmt.Sprintf("clones %s of restore %s, kept after a failed post-restore command, dropped",
		strings.Join(pr.ClonesKept, ", "), id), func(e *events.Event) { e.RestoreID, e.Database = id, rec.TargetDatabase })
	return rec, nil
}

// keptCloneOf reports whether name may be dropped as a kept clone of rec.
func keptCloneOf(rec *models.RestoreRecord, name string) bool {
	if name == "" || !models.IsRescueClone(name) || name == rec.SourceDatabase || rec.InPlace {
		return false
	}
	if rec.PITR != nil {
		if !slices.Contains(rec.PITR.Clones, name) {
			return false
		}
		src, ok := strings.CutSuffix(name, rec.PITR.CloneSuffix)
		return ok && src != "" && src != name
	}
	return name == rec.TargetDatabase
}

// commandKey identifies a post-restore command by its database and its compacted
// document.
func commandKey(c models.PostRestoreCommand) string {
	var b bytes.Buffer
	if json.Compact(&b, c.Command) != nil {
		return c.Database + "\x00" + string(c.Command)
	}
	return c.Database + "\x00" + b.String()
}

// sameCommands reports whether a and b hold the same commands in the same order.
func sameCommands(a, b []models.PostRestoreCommand) bool {
	return slices.EqualFunc(a, b, func(x, y models.PostRestoreCommand) bool { return commandKey(x) == commandKey(y) })
}

// removedCommands counts the commands of old that next no longer holds (removed or
// changed); reordering and additions count nothing.
func removedCommands(old, next []models.PostRestoreCommand) int {
	have := map[string]int{}
	for _, c := range next {
		have[commandKey(c)]++
	}
	removed := 0
	for _, c := range old {
		k := commandKey(c)
		if have[k] > 0 {
			have[k]--
			continue
		}
		removed++
	}
	return removed
}
