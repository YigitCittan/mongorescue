package models

import "time"

// ChunkCopy is the copy of a PITR oplog chunk on a copy target of its stream
// (pitr.Stream.CopyTargets): the same bytes under the same storage key, checked
// against the chunk's SHA-256, with the copy target's own Object Lock. Its state
// is a BackupCopy's.
type ChunkCopy struct {
	// ChunkID and StreamID identify the chunk.
	ChunkID  string `json:"chunk_id"`
	StreamID string `json:"stream_id"`
	BackupCopy
}

// CopyDue reports whether copy c waits in the copy queue and is due at now: pending
// without or past its next attempt, or failed with a next attempt that has passed.
func CopyDue(c *BackupCopy, now time.Time) bool {
	switch c.Status {
	case CopyPending:
		return c.NextAttemptAt == nil || !now.Before(*c.NextAttemptAt)
	case CopyFailed:
		return c.NextAttemptAt != nil && !now.Before(*c.NextAttemptAt)
	}
	return false
}
