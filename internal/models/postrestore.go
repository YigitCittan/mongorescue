package models

import (
	"encoding/json"
	"time"
)

// PostRestoreAllDatabases is the PostRestoreCommand.Database that runs a command in
// every database a restore created.
const PostRestoreAllDatabases = "*"

// PostRestoreCommand is a MongoDB command a connection runs against the databases a
// safe-clone restore into it created, before the restore is reported complete; the
// use case is re-applying erasures to restored data. See internal/postrestore for
// the commands that are allowed.
type PostRestoreCommand struct {
	// Database is the source database whose restored clone the command runs in, or
	// PostRestoreAllDatabases for every restored clone. A command never runs in the
	// database it names: only in the clone a restore created from it.
	Database string `json:"database"`
	// Command is the command document as (extended) JSON, e.g. {"delete": "users",
	// "deletes": [{"q": {"_id": 42}, "limit": 0}]}; its first key is the command name.
	Command json.RawMessage `json:"command"`
}

// PostRestoreStatus is the outcome of a restore's post-restore commands.
type PostRestoreStatus string

// Outcomes of the post-restore commands of a restore.
const (
	// PostRestoreCompleted means every command ran and succeeded.
	PostRestoreCompleted PostRestoreStatus = "completed"
	// PostRestoreFailed means a command failed (or could not run): the restore
	// failed and its clones were kept for inspection.
	PostRestoreFailed PostRestoreStatus = "failed"
	// PostRestoreSkipped means the restore runs no command: a dry run, or an
	// in-place restore.
	PostRestoreSkipped PostRestoreStatus = "skipped"
)

// Outcomes of one post-restore command (PostRestoreResult.Status).
const (
	// PostRestoreCommandOK is a command that ran and succeeded.
	PostRestoreCommandOK = "ok"
	// PostRestoreCommandFailed is a command that ran and failed.
	PostRestoreCommandFailed = "failed"
	// PostRestoreCommandPlanned is a command a restore would run (preflights, dry
	// runs).
	PostRestoreCommandPlanned = "planned"
	// PostRestoreCommandNotRun is a command left out after an earlier one failed.
	PostRestoreCommandNotRun = "not_run"
)

// PostRestoreCounts are the counts a post-restore command reported. They never
// carry document contents.
type PostRestoreCounts struct {
	// N is the number of documents the command matched (update, findAndModify) or
	// deleted (delete).
	N int64 `json:"n"`
	// Modified is the number of documents an update modified.
	Modified int64 `json:"modified,omitempty"`
	// Upserted is the number of documents an update or findAndModify inserted.
	Upserted int64 `json:"upserted,omitempty"`
}

// PostRestoreResult is one post-restore command run (or planned) in one restored
// database. It records the command name and its counts, never the command
// document or the documents it touched.
type PostRestoreResult struct {
	// Index is the command's position in the connection's list (from 0).
	Index int `json:"index"`
	// Database is the restored clone the command runs in; SourceDatabase the
	// database it was restored from.
	Database       string `json:"database"`
	SourceDatabase string `json:"source_database"`
	// Command is the command name (e.g. "delete") and Collection its collection.
	Command    string `json:"command"`
	Collection string `json:"collection"`
	// Status is PostRestoreCommandOK, PostRestoreCommandFailed,
	// PostRestoreCommandPlanned or PostRestoreCommandNotRun.
	Status string `json:"status"`
	PostRestoreCounts
	// DurationMS is how long the command took.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Error is the redacted reason of a failed command.
	Error string `json:"error,omitempty"`
}

// PostRestoreReport is what the post-restore commands of a restore did.
type PostRestoreReport struct {
	// Status is the overall outcome.
	Status PostRestoreStatus `json:"status"`
	// Commands lists every command per restored database, in the order they run.
	Commands []PostRestoreResult `json:"commands,omitempty"`
	// Note explains a skipped or failed run.
	Note string `json:"note,omitempty"`
	// ClonesKept names the restored databases a failed run left for inspection:
	// they hold the restored data without every command applied and must not be
	// used.
	ClonesKept []string `json:"clones_kept,omitempty"`
	// ClonesDroppedAt and ClonesDroppedBy say when and by whom the kept clones were
	// dropped (POST /api/v1/restores/{id}/drop-clones).
	ClonesDroppedAt *time.Time `json:"clones_dropped_at,omitempty"`
	ClonesDroppedBy string     `json:"clones_dropped_by,omitempty"`
}

// KeepsClones reports whether a failed run left clones that were not dropped yet.
func (r *PostRestoreReport) KeepsClones() bool {
	return r != nil && len(r.ClonesKept) > 0 && r.ClonesDroppedAt == nil
}

// ClonePostRestoreCommands returns a deep copy of cmds (nil for none).
func ClonePostRestoreCommands(cmds []PostRestoreCommand) []PostRestoreCommand {
	if len(cmds) == 0 {
		return nil
	}
	out := make([]PostRestoreCommand, len(cmds))
	for i, c := range cmds {
		out[i] = PostRestoreCommand{Database: c.Database, Command: append(json.RawMessage(nil), c.Command...)}
	}
	return out
}
