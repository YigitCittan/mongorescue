package pitr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Errors of PlanRestore. Each is wrapped with the details of the refusal.
var (
	// ErrInvalidTarget is returned for a restore target that sets neither or both of
	// a time and an exact timestamp, or one out of range.
	ErrInvalidTarget = errors.New("pitr: invalid restore target")
	// ErrOutsideWindow is returned for a target before the earliest eligible base or
	// after the newest collected oplog entry: no window holds it.
	ErrOutsideWindow = errors.New("pitr: the target is outside every point-in-time window")
	// ErrChainBreak is returned when the oplog between a base and the target is not
	// one unbroken run of chunks (a gap, a divergence or a missing chunk).
	ErrChainBreak = errors.New("pitr: the oplog chain is broken between the base and the target")
	// ErrChunkFailed is returned when a chunk the restore needs failed verification.
	ErrChunkFailed = errors.New("pitr: an oplog chunk in the range failed verification")
	// ErrKeyMissing is returned when the base or a chunk is encrypted in a mode for
	// which no decryption key is configured.
	ErrKeyMissing = errors.New("pitr: no decryption key for the encryption of the range")
)

// Target is the moment a point-in-time restore goes back to: a wall-clock second
// At (the primary's clock) or an exact oplog position TS. Exactly one is set.
type Target struct {
	// At restores every write up to and including the second At (truncated to the
	// second): the replay stops at (At+1):0.
	At time.Time
	// TS restores every write before the entry at TS: the replay stops at TS, which
	// is not applied.
	TS *Timestamp
}

// Limit returns the exclusive end of the replay, the --oplogLimit position: (S+1):0
// for a time S, or the exact timestamp. It returns ErrInvalidTarget unless exactly
// one of At and TS is set and in range.
func (t Target) Limit() (Timestamp, error) {
	switch {
	case t.At.IsZero() == (t.TS == nil):
		return Timestamp{}, fmt.Errorf("%w: set exactly one of a time and an exact timestamp", ErrInvalidTarget)
	case t.TS != nil:
		if t.TS.IsZero() {
			return Timestamp{}, fmt.Errorf("%w: the timestamp is zero", ErrInvalidTarget)
		}
		return *t.TS, nil
	}
	s := t.At.Unix()
	if s <= 0 || s >= math.MaxUint32 {
		return Timestamp{}, fmt.Errorf("%w: the time %s is out of range", ErrInvalidTarget, t.At.UTC().Format(time.RFC3339))
	}
	return Timestamp{T: uint32(s) + 1}, nil
}

// Base is a completed base backup of a stream, as the planner sees it: the caller
// passes only instance-scope bases of the stream with both T_before and T_after.
type Base struct {
	// ID is the backup ID.
	ID string `json:"id"`
	// TBefore and TAfter are the last write before and after the dump; TAfter is
	// the base's consistent point.
	TBefore OpTime `json:"t_before"`
	TAfter  OpTime `json:"t_after"`
	// SizeBytes is the size of the stored archive.
	SizeBytes int64 `json:"size_bytes"`
	// Encrypted and EncryptionMode describe the archive's encryption.
	Encrypted      bool   `json:"encrypted"`
	EncryptionMode string `json:"encryption_mode,omitempty"`
	// StartedAt is when the base backup started.
	StartedAt time.Time `json:"started_at"`
}

// PlanSource is what PlanRestore reads.
type PlanSource struct {
	// Repo lists the stream's chains and chunks.
	Repo Repository
	// Bases are the completed instance-scope bases of the stream.
	Bases []Base
	// HasKey reports whether a decryption key for an encryption mode (such as
	// "x25519" or "scrypt") is configured; nil means none is.
	HasKey func(mode string) bool
}

// RestorePlan is the base and the oplog chunks a point-in-time restore replays.
type RestorePlan struct {
	// StreamID and ChainID are the stream and the chain the restore reads.
	StreamID string `json:"stream_id"`
	ChainID  string `json:"chain_id"`
	// Base is the base backup restored first (pass 1).
	Base Base `json:"base"`
	// Chunks are the chunks replayed in pass 2, in order: from the one holding
	// T_before up to the one that reaches Limit.
	Chunks []*Chunk `json:"-"`
	// ChunkCount and OplogBytes count Chunks and their stored bytes.
	ChunkCount int   `json:"chunk_count"`
	OplogBytes int64 `json:"oplog_bytes"`
	// UnverifiedChunks counts the chunks never verified; they are hash-checked
	// while they stream, like every chunk.
	UnverifiedChunks int `json:"unverified_chunks"`
	// Limit is the exclusive end of the replay (--oplogLimit).
	Limit Timestamp `json:"limit"`
	// TargetTime is the wall-clock time of the target: At, or the second of TS.
	TargetTime time.Time `json:"target_time"`
}

// PlanRestore chooses the newest base backup of stream from which the oplog reaches
// target in one unbroken chain: a base whose consistent point T_after is before the
// target, where the live committed chunks run without a break from T_before past
// the target, none failed verification (chunks never verified are accepted: they are
// hash-checked while they stream) and a key is configured for the encryption of the
// base and of every chunk.
//
// It returns ErrInvalidTarget, ErrOutsideWindow (no base before the target, or the
// target after the newest chunk), ErrChainBreak, ErrChunkFailed or ErrKeyMissing.
// When no base qualifies, the reason found for the newest candidate is returned.
func PlanRestore(ctx context.Context, stream *Stream, src PlanSource, target Target) (*RestorePlan, error) {
	if stream == nil {
		return nil, fmt.Errorf("%w: stream", ErrNotFound)
	}
	limit, err := target.Limit()
	if err != nil {
		return nil, err
	}
	chains, err := src.Repo.ListChains(ctx, stream.ID)
	if err != nil {
		return nil, fmt.Errorf("pitr: list chains: %w", err)
	}
	byChain := make(map[string][]*Chunk, len(chains))
	var newest Timestamp
	for _, c := range chains {
		chunks, listErr := src.Repo.ListChunks(ctx, ChunkQuery{StreamID: stream.ID, ChainID: c.ChainID, Status: ChunkCommitted, Live: true})
		if listErr != nil {
			return nil, fmt.Errorf("pitr: list chunks of chain %s: %w", c.ChainID, listErr)
		}
		live := chunks[:0]
		for _, ch := range chunks {
			if ch.Live() {
				live = append(live, ch)
			}
		}
		byChain[c.ChainID] = live
		if n := len(live); n > 0 && live[n-1].To.Compare(newest) > 0 {
			newest = live[n-1].To
		}
	}
	if limit.Compare(newest) > 0 {
		if newest.IsZero() {
			return nil, fmt.Errorf("%w: the stream has no oplog chunks", ErrOutsideWindow)
		}
		return nil, fmt.Errorf("%w: the newest collected oplog entry is %s (%s); choose a time before it",
			ErrOutsideWindow, newest, newest.Time().Format(time.RFC3339))
	}

	bases := make([]Base, 0, len(src.Bases))
	for _, b := range src.Bases {
		if b.TAfter.TS.Compare(limit) < 0 && !b.TBefore.TS.IsZero() {
			bases = append(bases, b)
		}
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("%w: no base backup has a consistent point before the target", ErrOutsideWindow)
	}
	sort.SliceStable(bases, func(i, j int) bool { return bases[i].TAfter.TS.Compare(bases[j].TAfter.TS) > 0 })

	var firstErr error
	for _, b := range bases {
		plan, planErr := planFrom(chains, byChain, b, limit)
		if planErr == nil {
			plan.StreamID = stream.ID
			plan.TargetTime = target.At.UTC().Truncate(time.Second)
			if target.TS != nil {
				plan.TargetTime = target.TS.Time()
			}
			if keyErr := checkKeys(plan, src.HasKey); keyErr != nil {
				return nil, keyErr
			}
			return plan, nil
		}
		if firstErr == nil {
			firstErr = planErr
		}
	}
	return nil, firstErr
}

// planFrom returns the plan that restores base b and replays up to limit, from the
// chain whose chunks cover b.
func planFrom(chains []*Chain, byChain map[string][]*Chunk, b Base, limit Timestamp) (*RestorePlan, error) {
	var firstErr error
	for _, c := range chains {
		chunks := byChain[c.ChainID]
		start := -1
		for i, ch := range chunks {
			if ch.To.Compare(b.TBefore.TS) >= 0 {
				if ch.From.Compare(b.TBefore.TS) <= 0 {
					start = i
				}
				break
			}
		}
		if start < 0 {
			continue
		}
		plan := &RestorePlan{ChainID: c.ChainID, Base: b, Limit: limit}
		var termOK bool
		for i := start; i < len(chunks); i++ {
			ch := chunks[i]
			if i > start && ch.From != chunks[i-1].To {
				return nil, fmt.Errorf("%w: chain %s has no chunk from %s to %s", ErrChainBreak, c.ChainID, chunks[i-1].To, ch.From)
			}
			if ch.VerifyError != "" {
				return nil, fmt.Errorf("%w: chunk %s (%s-%s): %s", ErrChunkFailed, ch.ID, ch.From, ch.To, ch.VerifyError)
			}
			if !termOK && ch.From.Compare(b.TAfter.TS) < 0 && ch.To.Compare(b.TAfter.TS) >= 0 {
				if b.TAfter.Term != 0 && (b.TAfter.Term < ch.FirstTerm || b.TAfter.Term > ch.LastTerm) {
					return nil, fmt.Errorf("%w: the consistent point of base %s (term %d) is not in chain %s (rolled back)",
						ErrChainBreak, b.ID, b.TAfter.Term, c.ChainID)
				}
				termOK = true
			}
			plan.Chunks = append(plan.Chunks, ch)
			plan.OplogBytes += ch.SizeBytes
			if ch.VerifiedAt == nil {
				plan.UnverifiedChunks++
			}
			if ch.To.Compare(limit) >= 0 {
				if !termOK && b.TAfter.TS.Compare(plan.Chunks[0].From) > 0 {
					return nil, fmt.Errorf("%w: chain %s does not hold the consistent point of base %s", ErrChainBreak, c.ChainID, b.ID)
				}
				plan.ChunkCount = len(plan.Chunks)
				return plan, nil
			}
		}
		end := Timestamp{}
		if n := len(chunks); n > 0 {
			end = chunks[n-1].To
		}
		firstErr = fmt.Errorf("%w: chain %s of base %s ends at %s (%s), before the target",
			ErrChainBreak, c.ChainID, b.ID, end, end.Time().Format(time.RFC3339))
		break
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("%w: no oplog chain covers base %s from its start", ErrChainBreak, b.ID)
	}
	return nil, firstErr
}

// checkKeys returns ErrKeyMissing when the base or a chunk of plan is encrypted in
// a mode hasKey does not cover.
func checkKeys(plan *RestorePlan, hasKey func(mode string) bool) error {
	missing := func(encrypted bool, mode string) bool {
		return encrypted && (hasKey == nil || !hasKey(mode))
	}
	if missing(plan.Base.Encrypted, plan.Base.EncryptionMode) {
		return fmt.Errorf("%w: base %s is encrypted (%s)", ErrKeyMissing, plan.Base.ID, modeName(plan.Base.EncryptionMode))
	}
	for _, ch := range plan.Chunks {
		if missing(ch.Encrypted, ch.EncryptionMode) {
			return fmt.Errorf("%w: chunk %s is encrypted (%s)", ErrKeyMissing, ch.ID, modeName(ch.EncryptionMode))
		}
	}
	return nil
}

// modeName names an encryption mode for messages.
func modeName(mode string) string {
	if mode == "" {
		return "unknown mode"
	}
	return mode
}
