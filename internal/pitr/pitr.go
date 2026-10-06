// Package pitr holds the domain model of point-in-time recovery for replica sets:
// PITR streams (one per connection), the chains of oplog chunks a collector writes,
// the chunks themselves and the collector's persisted position. See
// docs/design/pitr.md.
//
// The package is the domain core and imports no MongoDB driver: persistence
// (Repository) is a port implemented by internal/store, and oplog reads are served
// by internal/mongoconn, which returns the OplogWindow and OplogStats defined here.
// The collector service is internal/pitr/collector.
package pitr

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors.
var (
	// ErrNotFound is returned for an unknown stream, chain or collector state.
	ErrNotFound = errors.New("pitr: not found")
	// ErrAlreadyExists is returned when creating a stream or chain whose ID is
	// taken, or committing a chunk whose storage key is already recorded on its
	// target.
	ErrAlreadyExists = errors.New("pitr: already exists")
	// ErrConnectionTaken is returned when creating or moving a stream to a
	// connection that already has one: a connection has at most one PITR stream.
	ErrConnectionTaken = errors.New("pitr: the connection already has a PITR stream")
	// ErrChainOpen is returned when starting a chain while the stream still has an
	// open one: end it first.
	ErrChainOpen = errors.New("pitr: the stream already has an open chain")
	// ErrChainEnded is returned when ending a chain twice or committing a chunk to a
	// chain that has ended or is not the stream's current chain.
	ErrChainEnded = errors.New("pitr: the chain has ended or is not the stream's current chain")
	// ErrDiscontinuous is returned when a chunk does not start exactly where the
	// collector's stored position ends, which would leave a gap or an overlap.
	ErrDiscontinuous = errors.New("pitr: the chunk does not continue the chain")
	// ErrInUse is returned when deleting a stream that still has chunks in storage
	// (committed or superseded): retention must prune them first.
	ErrInUse = errors.New("pitr: the stream still has oplog chunks in storage")
	// ErrNotReplicaSet is returned by oplog reads from a server that is not a
	// replica set member: a standalone server has no oplog.
	ErrNotReplicaSet = errors.New("pitr: the server is not a replica set member")
	// ErrOplogBehind is returned by a range read that ended before its upper bound:
	// the member that answered has not replicated up to it yet. Nothing read may be
	// stored; read the range again.
	ErrOplogBehind = errors.New("pitr: the member's oplog does not reach the end of the range yet")
	// ErrOplogGap is returned by a range read whose start entry is missing on the
	// member that served it (truncated: the window was overrun) or has another term
	// (rolled back or diverged). Nothing read may be stored; the collector re-checks
	// the window and ends the chain as a gap or a divergence.
	ErrOplogGap = errors.New("pitr: the oplog no longer holds the start of the range")
)

// Timestamp is a BSON timestamp, the position of an oplog entry: T is the seconds
// since the Unix epoch and I the ordinal within that second.
type Timestamp struct {
	T uint32 `json:"t"`
	I uint32 `json:"i"`
}

// Compare returns -1, 0 or +1 as ts is before, equal to or after o.
func (ts Timestamp) Compare(o Timestamp) int {
	if c := cmp.Compare(ts.T, o.T); c != 0 {
		return c
	}
	return cmp.Compare(ts.I, o.I)
}

// IsZero reports whether ts is the zero timestamp.
func (ts Timestamp) IsZero() bool { return ts.T == 0 && ts.I == 0 }

// Time returns the wall-clock second of ts (the primary's clock), in UTC.
func (ts Timestamp) Time() time.Time { return time.Unix(int64(ts.T), 0).UTC() }

// String renders ts as "T.I" with both parts zero-padded to ten digits, so the
// strings sort in time order (the form chunk keys use).
func (ts Timestamp) String() string { return fmt.Sprintf("%010d.%010d", ts.T, ts.I) }

// OpTime is the position of an oplog entry together with the replica set term
// (election) it was written in. (ts, t) identifies an entry across rollbacks:
// h is always 0 since MongoDB 4.2.
type OpTime struct {
	TS   Timestamp `json:"ts"`
	Term int64     `json:"t"`
}

// OplogWindow describes the oplog of a replica set member at one moment.
type OplogWindow struct {
	// Oldest and Newest are the timestamps of the oldest and newest entries of
	// local.oplog.rs on the member that answered.
	Oldest, Newest Timestamp
	// MajorityOpTime is hello.lastWrite.majorityOpTime: the newest entry that is
	// majority-committed, the upper bound of a collector read.
	MajorityOpTime OpTime
	// ReplicaSet is the replica set name (hello.setName).
	ReplicaSet string
	// ReplicaSetID is the replica set ID (the hex ObjectID of
	// replSetGetConfig's settings.replicaSetId), which changes when a set is
	// re-initiated under the same name. It is empty when the user may not read the
	// configuration.
	ReplicaSetID string
}

// OplogRange is one range read of the oplog: the entries in (From, To], or
// [From, To] with StartInclusive. Every read proves its own continuity on the member
// that serves it: the entry at From must still be there (and have FromTerm when
// CheckTerm is set), otherwise the read fails with ErrOplogGap. To must be the
// timestamp of an existing entry, such as OplogWindow's MajorityOpTime.TS.
type OplogRange struct {
	// From is the position the range continues from: the end of the previous
	// chunk, or a chain's start.
	From Timestamp
	// To is the last position of the range (inclusive).
	To Timestamp
	// CheckTerm makes the read also require the entry at From to have FromTerm.
	CheckTerm bool
	// FromTerm is the stored term of the entry at From (see CheckTerm).
	FromTerm int64
	// StartInclusive also writes the entry at From. It is meant for the first
	// chunk of a new chain, which starts at an entry the collector has just seen
	// (such as the oldest entry of the window) rather than at a stored position.
	StartInclusive bool
}

// OplogStats summarises one range read of the oplog.
type OplogStats struct {
	// Entries is the number of oplog entries written.
	Entries int
	// First and Last are the positions of the first and last entries written; both
	// are zero when the range was empty.
	First, Last OpTime
}

// Stream is the PITR configuration of one replica set connection.
type Stream struct {
	// ID identifies the stream.
	ID string `json:"id"`
	// ConnectionID is the managed connection the stream collects from (unique).
	ConnectionID string `json:"connection_id"`
	// ReplicaSet is the replica set name the stream was set up for.
	ReplicaSet string `json:"replica_set"`
	// TargetID is the storage target chunks and base backups are written to.
	TargetID string `json:"target_id"`
	// Enabled turns the collector on.
	Enabled bool `json:"enabled"`
	// BaseCron is the cron schedule of base backups.
	BaseCron string `json:"base_cron"`
	// BaseKeepCount and BaseKeepDays are the base retention: keep this many bases,
	// or bases this many days old.
	BaseKeepCount int `json:"base_keep_count"`
	BaseKeepDays  int `json:"base_keep_days"`
	// OplogMaxDays caps how long chunks are kept (privacy); 0 means unset.
	OplogMaxDays int `json:"oplog_max_days"`
	// ChunkSeconds is the chunk interval in seconds.
	ChunkSeconds int `json:"chunk_seconds"`
	// BaseOnGap takes a base backup as soon as a gap ends a chain.
	BaseOnGap bool `json:"base_on_gap"`
	// ReadPreference is the read preference of oplog reads; empty means
	// secondaryPreferred.
	ReadPreference string `json:"read_preference,omitempty"`
	// ChainTestCron is the cron schedule of chain tests: a point-in-time restore
	// from one base to the consistent point of the next into temporary clones,
	// compared with that base's manifest. Empty turns chain tests off.
	ChainTestCron string `json:"chain_test_cron,omitempty"`
	// ChainTestConnectionID is the connection chain tests restore into; empty means
	// the stream's own connection. Another server spares production the load.
	ChainTestConnectionID string `json:"chain_test_connection_id,omitempty"`
	// CreatedAt and UpdatedAt are set by the repository.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EndReason tells why a chain ended.
type EndReason string

// Chain end reasons.
const (
	// EndGap: the oplog window was overrun, so entries were lost.
	EndGap EndReason = "gap"
	// EndReplicaSetChanged: the replica set name or ID changed.
	EndReplicaSetChanged EndReason = "replica_set_changed"
	// EndDiverged: the entry at the stored position vanished or changed term
	// (forced reconfiguration, majority data loss).
	EndDiverged EndReason = "diverged"
	// EndStopped: the stream was disabled or reconfigured.
	EndStopped EndReason = "stopped"
)

// Chain is an unbroken run of chunks of one stream: each chunk starts where the
// previous one ends. A gap or a divergence ends a chain and starts a new one.
type Chain struct {
	// StreamID and ChainID identify the chain.
	StreamID string `json:"stream_id"`
	ChainID  string `json:"chain_id"`
	// Start is the position the first chunk continues from (exclusive).
	Start Timestamp `json:"start"`
	// End is the position the chain ended at; zero while it is open.
	End Timestamp `json:"end"`
	// EndReason is empty while the chain is open.
	EndReason EndReason `json:"end_reason,omitempty"`
	// EndedAt is nil while the chain is open.
	EndedAt *time.Time `json:"ended_at,omitempty"`
}

// Open reports whether the chain has not ended.
func (c *Chain) Open() bool { return c.EndedAt == nil }

// ChunkStatus is the lifecycle state of a chunk.
type ChunkStatus string

// Chunk statuses.
const (
	// ChunkCommitted: the chunk is stored and part of its chain.
	ChunkCommitted ChunkStatus = "committed"
	// ChunkSuperseded: the chunk follows a divergence point and must not be
	// replayed; its object stays in storage until retention removes it.
	ChunkSuperseded ChunkStatus = "superseded"
	// ChunkPruned: retention deleted the chunk's object.
	ChunkPruned ChunkStatus = "pruned"
)

// Chunk is one stored range (From, To] of the oplog.
type Chunk struct {
	// ID identifies the chunk.
	ID string `json:"id"`
	// StreamID and ChainID are the stream and chain the chunk belongs to.
	StreamID string `json:"stream_id"`
	ChainID  string `json:"chain_id"`
	// TargetID and StorageKey locate the object (unique together).
	TargetID   string `json:"target_id"`
	StorageKey string `json:"storage_key"`
	// From (exclusive) and To (inclusive) bound the range read.
	From Timestamp `json:"from"`
	To   Timestamp `json:"to"`
	// FirstTerm and LastTerm are the terms of the first and last entries; an empty
	// chunk carries the term of the position it continues from in both.
	FirstTerm int64 `json:"first_term"`
	LastTerm  int64 `json:"last_term"`
	// Entries is the number of oplog entries in the chunk.
	Entries int64 `json:"entries"`
	// SizeBytes is the size of the stored object.
	SizeBytes int64 `json:"size_bytes"`
	// SHA256 is the hex SHA-256 of the stored object.
	SHA256 string `json:"sha256"`
	// Encrypted and EncryptionMode describe the object's encryption.
	Encrypted      bool   `json:"encrypted"`
	EncryptionMode string `json:"encryption_mode,omitempty"`
	// Status is set to committed by CommitChunk.
	Status ChunkStatus `json:"status"`
	// CreatedAt is when the chunk was committed.
	CreatedAt time.Time `json:"created_at"`
	// VerifiedAt and VerifyError are the outcome of the last verification.
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	VerifyError string     `json:"verify_error,omitempty"`
	// DeletedAt and PurgeAfter are set once retention deleted the chunk: it is no
	// longer part of any window, and its object stays until PurgeAfter.
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
	PurgeAfter *time.Time `json:"purge_after,omitempty"`
	// VersionID is the S3 version of the object on a versioned bucket (S3 Object
	// Lock): reads and the purge address it.
	VersionID string `json:"version_id,omitempty"`
	// RetainUntil is the end of the object's S3 Object Lock retention: a deleted
	// chunk is purged only afterwards.
	RetainUntil *time.Time `json:"retain_until,omitempty"`
}

// Live reports whether the chunk is part of its chain: committed and not deleted.
func (c *Chunk) Live() bool { return c.Status == ChunkCommitted && c.DeletedAt == nil }

// ChainSpan is the span of the live committed chunks of one chain.
type ChainSpan struct {
	// ChainID identifies the chain.
	ChainID string `json:"chain_id"`
	// From is the start of the oldest live chunk and To the end of the newest; both
	// are zero without chunks.
	From Timestamp `json:"from"`
	To   Timestamp `json:"to"`
	// Chunks and SizeBytes count the live chunks and their stored bytes.
	Chunks    int64 `json:"chunks"`
	SizeBytes int64 `json:"size_bytes"`
}

// CollectorStatus is the state of a stream's collector.
type CollectorStatus string

// Collector statuses.
const (
	// CollectorStopped: the collector is not running.
	CollectorStopped CollectorStatus = "stopped"
	// CollectorRunning: the collector is reading and storing chunks.
	CollectorRunning CollectorStatus = "running"
	// CollectorFailed: the last attempt failed (see State.LastError).
	CollectorFailed CollectorStatus = "failed"
)

// State is the persisted position of a stream's collector: a restart resumes from
// Last on chain ChainID.
type State struct {
	// StreamID identifies the stream.
	StreamID string `json:"stream_id"`
	// ChainID is the stream's current chain.
	ChainID string `json:"chain_id"`
	// Last is the end of the last committed chunk (or the chain's start) and the
	// term of the entry there, checked for divergence at start.
	Last OpTime `json:"last"`
	// Status and LastError describe the collector.
	Status    CollectorStatus `json:"status"`
	LastError string          `json:"last_error,omitempty"`
	// LagSince is when the lag first exceeded its threshold; nil while it does not.
	LagSince *time.Time `json:"lag_since,omitempty"`
	// WindowLowSince is when the oplog headroom dropped below its threshold; nil
	// while it does not.
	WindowLowSince *time.Time `json:"window_low_since,omitempty"`
	// ReplicaSetID is the replica set ID the collector first saw (see
	// OplogWindow.ReplicaSetID); empty until then or when it cannot be read.
	ReplicaSetID string `json:"replica_set_id,omitempty"`
	// UpdatedAt is when the state was last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// ChunkQuery selects the chunks of one chain whose range overlaps (After, Until].
type ChunkQuery struct {
	// StreamID and ChainID select the chain (both required).
	StreamID string
	ChainID  string
	// After keeps chunks whose To is after it; zero keeps all.
	After Timestamp
	// Until keeps chunks whose From is before it; zero means no upper bound.
	Until Timestamp
	// Status keeps only chunks in this status; empty keeps every status.
	Status ChunkStatus
	// Live drops deleted and pruned chunks.
	Live bool
}

// Repository is the persistence port of PITR streams, chains, chunks and collector
// state. Implementations return copies callers may mutate.
type Repository interface {
	// CreateStream inserts s, setting CreatedAt and UpdatedAt. It returns
	// ErrAlreadyExists when the ID is taken and ErrConnectionTaken when the
	// connection already has a stream.
	CreateStream(ctx context.Context, s *Stream) error
	// UpdateStream replaces s, setting UpdatedAt and keeping CreatedAt. It returns
	// ErrNotFound for a deleted stream and ErrConnectionTaken when moving it to a
	// connection that has a stream.
	UpdateStream(ctx context.Context, s *Stream) error
	// GetStream returns a stream or ErrNotFound.
	GetStream(ctx context.Context, id string) (*Stream, error)
	// GetStreamByConnection returns the stream of a connection or ErrNotFound.
	GetStreamByConnection(ctx context.Context, connectionID string) (*Stream, error)
	// ListStreams returns every stream ordered by ID.
	ListStreams(ctx context.Context) ([]*Stream, error)
	// DeleteStream removes a stream with its chains, state and pruned chunk rows.
	// It returns ErrNotFound for an unknown stream and ErrInUse, deleting nothing,
	// while committed or superseded chunks remain.
	DeleteStream(ctx context.Context, id string) error

	// StartChain inserts an open chain starting at start.TS and, in the same
	// transaction, points the stream's state at it (Last = start; a new state is
	// running). It returns ErrNotFound for an unknown stream, ErrChainOpen while
	// the stream has an open chain and ErrAlreadyExists for a taken chain ID.
	StartChain(ctx context.Context, streamID, chainID string, start OpTime, at time.Time) error
	// EndChain records that an open chain ended at end for reason. It returns
	// ErrNotFound for an unknown chain and ErrChainEnded for an ended one.
	EndChain(ctx context.Context, streamID, chainID string, end Timestamp, reason EndReason, at time.Time) error
	// ListChains returns the chains of a stream ordered by start.
	ListChains(ctx context.Context, streamID string) ([]*Chain, error)

	// CommitChunk inserts c as committed and, in the same transaction, moves the
	// stream's state to (c.To, c.LastTerm), running and without error. The chunk
	// must continue the state: same chain, which must be open, and c.From equal to
	// the stored position. It returns ErrNotFound without a state, ErrChainEnded,
	// ErrDiscontinuous, or ErrAlreadyExists for a recorded target and storage key.
	CommitChunk(ctx context.Context, c *Chunk) error
	// ListChunks returns the chunks selected by q ordered by From.
	ListChunks(ctx context.Context, q ChunkQuery) ([]*Chunk, error)
	// ListStreamChunks returns a page of the chunks of a stream, newest first, and
	// how many there are.
	ListStreamChunks(ctx context.Context, streamID string, limit, offset int) ([]*Chunk, int, error)
	// ChainSpans returns the span of the live committed chunks of every chain of a
	// stream, ordered by chain start.
	ChainSpans(ctx context.Context, streamID string) ([]ChainSpan, error)
	// ChunkKeys returns the storage keys of the chunks on a target whose object
	// is expected to exist (every status but pruned).
	ChunkKeys(ctx context.Context, targetID string) (map[string]bool, error)
	// MarkChunkVerified records the outcome of a chunk's verification.
	MarkChunkVerified(ctx context.Context, id string, at time.Time, verifyError string) error
	// DeleteChunks soft-deletes chunks (see Chunk.DeletedAt) and returns how many.
	DeleteChunks(ctx context.Context, ids []string, at, purgeAfter time.Time) (int64, error)
	// ListPurgeableChunks returns up to limit deleted chunks of a stream whose
	// grace period ended at now.
	ListPurgeableChunks(ctx context.Context, streamID string, now time.Time, limit int) ([]*Chunk, error)
	// MarkChunkPruned records that a deleted chunk's object was removed.
	MarkChunkPruned(ctx context.Context, id string) error
	// ListChunksToVerify returns up to limit live chunks last verified before
	// before, never verified ones first.
	ListChunksToVerify(ctx context.Context, before time.Time, limit int) ([]*Chunk, error)
	// SupersedeChunks marks the committed chunks of a chain whose To is after
	// after as superseded and returns how many it marked.
	SupersedeChunks(ctx context.Context, streamID, chainID string, after Timestamp) (int64, error)

	// LoadState returns a stream's collector state or ErrNotFound.
	LoadState(ctx context.Context, streamID string) (*State, error)
	// SetCollectorStatus records the collector's status, last error and lag start
	// without moving its position. It returns ErrNotFound without a state.
	SetCollectorStatus(ctx context.Context, streamID string, status CollectorStatus, lastError string, lagSince *time.Time, at time.Time) error
	// SetWindowLow records when the headroom of a stream dropped below its
	// threshold (nil: it is back above). It returns ErrNotFound without a state.
	SetWindowLow(ctx context.Context, streamID string, since *time.Time) error
	// SetReplicaSetID records the replica set ID the collector of a stream reads
	// from. It returns ErrNotFound without a state.
	SetReplicaSetID(ctx context.Context, streamID, replicaSetID string) error
}
