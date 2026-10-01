package models

import "time"

// RunKind tells backups and restores apart in run-level data (progress, logs).
type RunKind string

// Run kinds.
const (
	// RunBackup is a backup run.
	RunBackup RunKind = "backup"
	// RunRestore is a restore run.
	RunRestore RunKind = "restore"
)

// Phase names reported in RunProgress.Phase, in the order a run passes them.
const (
	// PhaseQueued is a run that is recorded but not started yet.
	PhaseQueued = "queued"
	// PhaseDumping is a backup streaming mongodump's output to storage.
	PhaseDumping = "dumping"
	// PhaseVerifying is a restore checking the stored artifact before mongorestore.
	PhaseVerifying = "verifying"
	// PhaseRestoring is a restore streaming the artifact into mongorestore.
	PhaseRestoring = "restoring"
	// PhaseFinishing is a run that wrote its data and is finalizing.
	PhaseFinishing = "finishing"
	// PhaseCancelling is a run whose cancellation was requested.
	PhaseCancelling = "cancelling"
)

// RunPhases holds the timestamps of a run's phases. Backups pass queued, started,
// dump done, upload done (and verify done, once verified) and finished; restores
// pass queued, started, verify done (when verified first), restore done and finished.
// Unreached phases are nil.
type RunPhases struct {
	// Queued is when the run was recorded.
	Queued *time.Time `json:"queued,omitempty"`
	// Started is when the engine started working on the run.
	Started *time.Time `json:"started,omitempty"`
	// DumpDone is when mongodump exited (backups).
	DumpDone *time.Time `json:"dump_done,omitempty"`
	// UploadDone is when the archive was completely written to storage (backups).
	UploadDone *time.Time `json:"upload_done,omitempty"`
	// VerifyDone is when the artifact was verified.
	VerifyDone *time.Time `json:"verify_done,omitempty"`
	// RestoreDone is when mongorestore exited (restores).
	RestoreDone *time.Time `json:"restore_done,omitempty"`
	// Finished is when the run ended, whatever the outcome.
	Finished *time.Time `json:"finished,omitempty"`
}

// IsZero reports whether no phase is set.
func (p RunPhases) IsZero() bool {
	return p == RunPhases{}
}

// Stamp returns a pointer to t in UTC, for setting a phase.
func Stamp(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

// RunProgress is the live progress of a running backup or restore. Counts come from
// the tools' progress lines and from the bytes streamed to or from storage.
type RunProgress struct {
	// ID is the backup or restore record ID.
	ID string `json:"id"`
	// Kind is RunBackup or RunRestore.
	Kind RunKind `json:"kind"`
	// JobID is the job of a backup run, if any.
	JobID string `json:"job_id,omitempty"`
	// Database is the backed-up database, or the restore target.
	Database string `json:"database,omitempty"`
	// Phase is the current phase (see the Phase constants).
	Phase string `json:"phase"`
	// Percent is the estimated completion (0-100) when known.
	Percent *float64 `json:"percent,omitempty"`
	// Bytes counts the bytes streamed to (backups) or from (restores) storage in the
	// current phase.
	Bytes int64 `json:"bytes"`
	// TotalBytes is the size of the artifact a restore reads, when known.
	TotalBytes int64 `json:"total_bytes,omitempty"`
	// Documents counts the documents dumped or restored so far.
	Documents int64 `json:"documents"`
	// CurrentCollection is the namespace the tool reported last.
	CurrentCollection string `json:"current_collection,omitempty"`
	// CollectionsDone counts the collections the tool finished.
	CollectionsDone int `json:"collections_done"`
	// CollectionsTotal counts the collections the tool started so far.
	CollectionsTotal int `json:"collections_total"`
	// BytesPerSecond is the average throughput of the current phase.
	BytesPerSecond float64 `json:"bytes_per_second"`
	// StartedAt is when the run started.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is when the progress last changed.
	UpdatedAt time.Time `json:"updated_at"`
	// Cancelling reports that a cancellation was requested and the run is stopping.
	Cancelling bool `json:"cancelling,omitempty"`
	// Phases are the phase timestamps reached so far.
	Phases RunPhases `json:"phases,omitzero"`
}
