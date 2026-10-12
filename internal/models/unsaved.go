package models

import (
	"strings"
	"time"
)

// ErrRestoreRecordNotSaved starts the error of a restore whose final record could
// not be saved (see RestoreRecord.FailUnsaved).
const ErrRestoreRecordNotSaved = "the restore record could not be saved"

// FailUnsaved marks r failed because its final record could not be saved (cause,
// already redacted, says why; note, such as "; the clone x was dropped", says
// what happened to what it restored), so the run never reports a success the
// metadata does not record. A cancelled restore stays cancelled.
func (r *RestoreRecord) FailUnsaved(cause, note string, now time.Time) {
	if !strings.HasPrefix(r.ErrorMessage, ErrRestoreRecordNotSaved) {
		msg := ErrRestoreRecordNotSaved
		if cause != "" {
			msg += " (" + cause + ")"
		}
		msg += note
		if r.ErrorMessage != "" {
			msg += "; the run itself ended with: " + r.ErrorMessage
		}
		r.ErrorMessage = msg
	}
	if r.Status != RestoreStatusCancelled {
		r.Status = RestoreStatusFailed
	}
	if r.CompletedAt == nil {
		at := now.UTC()
		r.CompletedAt = &at
	}
}
