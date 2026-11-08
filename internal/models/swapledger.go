package models

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors of a SwapLedger.
var (
	// ErrSwapLedger is returned (wrapped) when the swap ledger cannot be read or
	// written: nothing may swap, or be settled, while it is unavailable.
	ErrSwapLedger = errors.New("the in-place PITR swap ledger of the HA cluster is unavailable")
	// ErrSwapMarkerExists is returned by SwapLedger.Begin for a restore that
	// already has a marker.
	ErrSwapMarkerExists = errors.New("an in-place PITR swap marker of this restore already exists")
)

// SwapPhase is how far an in-place point-in-time restore's swap got, as its
// ledger marker tells.
type SwapPhase string

const (
	// SwapPhaseSwapping: the swap runs (or its process died without telling).
	SwapPhaseSwapping SwapPhase = "swapping"
	// SwapPhaseFinished: the swapping process finished and wrote its outcome.
	SwapPhaseFinished SwapPhase = "finished"
)

// SwapOutcome is the outcome a finished swap reports.
type SwapOutcome string

const (
	// SwapCompleted: every original database holds the restored state.
	SwapCompleted SwapOutcome = "completed"
	// SwapFailed: the swap stopped; Swapped names the databases that hold the
	// restored state, and the clones were kept.
	SwapFailed SwapOutcome = "failed"
	// SwapAborted: the swap never started (the lease was lost first); every
	// original database is untouched.
	SwapAborted SwapOutcome = "aborted"
)

// SwapMarker is the entry of the swap ledger of one in-place point-in-time
// restore: written under the lease epoch Epoch right before the first original
// database is replaced, and completed by the same process once it stopped
// writing. It holds what another instance needs to settle the restore without
// the metadata the swapping instance had.
type SwapMarker struct {
	// RestoreID, StreamID and ConnectionID name the restore, its PITR stream and
	// the connection whose databases it replaces.
	RestoreID    string `json:"restore_id"`
	StreamID     string `json:"stream_id"`
	ConnectionID string `json:"connection_id"`
	// Databases are the original databases the swap replaces; Clones the clones it
	// copies from (kept when the swap does not complete).
	Databases []string `json:"databases"`
	Clones    []string `json:"clones,omitempty"`
	// StreamWasEnabled reports that the stream collected before the restore paused
	// it: whoever settles the restore enables it again.
	StreamWasEnabled bool `json:"stream_was_enabled,omitempty"`
	// KeepClone reports that the clones are kept after a completed swap.
	KeepClone bool `json:"keep_clone,omitempty"`
	// SafetyBackupIDs are the safety backups taken before the swap.
	SafetyBackupIDs []string `json:"safety_backup_ids,omitempty"`
	// Epoch is the HA epoch the swap runs under; InstanceID the instance.
	Epoch      uint64 `json:"epoch"`
	InstanceID string `json:"instance_id,omitempty"`
	// StartedAt is the swapping instance's clock when the marker was written, for
	// people only: no decision reads it.
	StartedAt time.Time `json:"started_at"`

	// Phase is SwapPhaseSwapping until the swapping process finishes. Outcome,
	// Swapped and FinishedAt are set with SwapPhaseFinished.
	Phase      SwapPhase   `json:"phase"`
	Outcome    SwapOutcome `json:"outcome,omitempty"`
	Swapped    []string    `json:"swapped,omitempty"`
	FinishedAt time.Time   `json:"finished_at,omitzero"`
}

// Finished reports whether the swapping process wrote its outcome.
func (m SwapMarker) Finished() bool { return m.Phase == SwapPhaseFinished }

// SwapLedger records the in-flight swaps of in-place point-in-time restores on
// the HA target (implemented by ha.SwapBoard), so that an instance that takes
// over from one that was swapping leaves the swap alone until its process says it
// stopped, or a bounded time passed (docs/design/ha.md, "In-place PITR").
type SwapLedger interface {
	// Begin writes the marker of a swap that is about to start and then checks the
	// fence: it fails with the fence's error (the marker marked aborted) when the
	// lease is lost, so no swap starts after a takeover that could not see its
	// marker.
	Begin(ctx context.Context, m SwapMarker) error
	// Finish records that the swap of restoreID stopped writing, with its outcome.
	// It is not fenced: it only tells the next active instance that the swap
	// stopped, and is written by the process that swapped, authenticated with the
	// cluster's key.
	Finish(ctx context.Context, restoreID string, outcome SwapOutcome, swapped []string) error
	// Open returns the authentic markers that were not resolved, whatever their
	// epoch. A marker that fails authentication is skipped (as if it had been
	// deleted, which whoever can write the HA target can do as well) and logged.
	Open(ctx context.Context) ([]SwapMarker, error)
	// Resolve removes the marker of restoreID once its swap was settled.
	Resolve(ctx context.Context, restoreID string) error
}
