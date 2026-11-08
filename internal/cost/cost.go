// Package cost tracks what MongoRescue stores and reads back, per storage target:
// the stored bytes of the archives it holds (primary archives, copies, retained
// copies and PITR oplog chunks) per target, job and database, a daily snapshot of
// them kept for a year, an estimated monthly cost from a price the operator
// entered on the target, and an estimate of the egress verification, restore tests
// and copies cause.
//
// Everything is computed from MongoRescue's own metadata. No cloud billing API is
// called, so the figures are estimates: they use stored (encrypted, compressed)
// sizes, ignore the provider's rounding, tiers, request fees and free allowances,
// and know only the last verification of an archive.
package cost

import (
	"context"
	"errors"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// Sentinel errors.
var (
	// ErrInvalid is returned for a request out of range.
	ErrInvalid = errors.New("cost: invalid request")
)

// Kind says what a stored object is.
type Kind string

// Kinds of stored objects.
const (
	// KindBackup is a primary backup archive.
	KindBackup Kind = "backup"
	// KindCopy is a copy of an archive or of an oplog chunk on a copy target.
	KindCopy Kind = "copy"
	// KindRetained is a copy that outlives its deleted primary because the job's
	// copy retention keeps it.
	KindRetained Kind = "retained"
	// KindPITR is an oplog chunk of a point-in-time recovery stream.
	KindPITR Kind = "pitr"
)

// EgressSource says why bytes were read back from a target.
type EgressSource string

// Sources of egress.
const (
	// EgressVerification is the re-read of an archive or copy to check its checksum.
	EgressVerification EgressSource = "verification"
	// EgressCopy is the read of a primary archive to write a copy.
	EgressCopy EgressSource = "copy"
	// EgressRestoreTest is the read of an archive by a restore test or DR drill.
	EgressRestoreTest EgressSource = "restore_test"
)

// Retention limits.
const (
	// HistoryRetentionDays is how long the daily snapshots are kept.
	HistoryRetentionDays = 400
	// DefaultDays is the range of a report without an explicit one.
	DefaultDays = 30
	// MaxDays is the longest range a report covers.
	MaxDays = HistoryRetentionDays
)

// DayLayout is the format of a snapshot's day (UTC).
const DayLayout = time.DateOnly

// Row is the stored size of one group of objects: the objects of one kind of one
// job and database (or PITR stream) on one storage target.
type Row struct {
	// TargetID is the storage target holding the objects.
	TargetID string `json:"target_id"`
	// Kind is what the objects are.
	Kind Kind `json:"kind"`
	// JobID is the job that took the backups; empty for manual backups and PITR.
	JobID string `json:"job_id,omitempty"`
	// Database is the backed-up database; empty for PITR chunks.
	Database string `json:"database,omitempty"`
	// StreamID is the PITR stream of oplog chunks; empty for archives.
	StreamID string `json:"stream_id,omitempty"`
	// Objects counts the objects.
	Objects int64 `json:"objects"`
	// Bytes is their total stored size.
	Bytes int64 `json:"bytes"`
}

// DayRow is a Row of one day's snapshot.
type DayRow struct {
	Row
	// Day is the UTC day, "2006-01-02".
	Day string
}

// Read is a read of bytes from a storage target that cost egress.
type Read struct {
	// TargetID is the target read from.
	TargetID string
	// Source is why it was read.
	Source EgressSource
	// Bytes is how many bytes.
	Bytes int64
	// At is when.
	At time.Time
}

// Repository is the persistence port of the cost tracker (implemented by
// internal/store).
type Repository interface {
	// LiveUsage aggregates the archives and chunks that exist now. A record without
	// a storage target has TargetID "".
	LiveUsage(ctx context.Context) ([]Row, error)
	// ReplaceUsageDay replaces the snapshot of day with rows, atomically.
	ReplaceUsageDay(ctx context.Context, day string, rows []Row) error
	// UsageHistory returns the snapshot rows of the days from through to
	// (inclusive), oldest first.
	UsageHistory(ctx context.Context, from, to string) ([]DayRow, error)
	// PruneUsage deletes the snapshots of the days before day and returns how many
	// rows it deleted.
	PruneUsage(ctx context.Context, before string) (int64, error)
	// EgressReads returns the reads from storage targets at or after since:
	// verifications, copy source reads and restore tests.
	EgressReads(ctx context.Context, since time.Time) ([]Read, error)
}

// TargetLister lists the storage targets (implemented by targets.Service).
type TargetLister interface {
	// List returns every storage target.
	List(ctx context.Context) ([]*models.StorageTarget, error)
}
