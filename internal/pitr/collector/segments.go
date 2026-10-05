package collector

import (
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// Segment is a run of usable chunks of one chain: live, committed, not failed
// verification, each starting where the previous one ends. A chunk whose
// verification failed (its object is missing or does not match) or a hole in the
// chain splits a chain into segments; no window crosses a split.
type Segment struct {
	// From is the start of the first chunk and To the end of the last.
	From pitr.Timestamp `json:"from"`
	To   pitr.Timestamp `json:"to"`
	// Chunks counts the segment's chunks.
	Chunks int64 `json:"chunks"`
	// chunks are the segment's chunks, for the term lookups of coverage.
	chunks []*pitr.Chunk
}

// segments splits the chunks of one chain (ordered by From) into segments; it
// returns them and the number of live chunks that failed verification.
func segments(chunks []*pitr.Chunk) (out []Segment, corrupt int) {
	var cur *Segment
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, c := range chunks {
		if !c.Live() {
			continue
		}
		if c.VerifyError != "" {
			corrupt++
			flush()
			continue
		}
		if cur != nil && c.From != cur.To {
			flush()
		}
		if cur == nil {
			cur = &Segment{From: c.From}
		}
		cur.To = c.To
		cur.Chunks++
		cur.chunks = append(cur.chunks, c)
	}
	flush()
	return out, corrupt
}

// termAt returns the terms of the chunk of seg that holds position ts (the chunk
// with From < ts <= To, or the first chunk when ts is its start).
func (seg Segment) termAt(ts pitr.Timestamp) (first, last int64, ok bool) {
	for _, c := range seg.chunks {
		if (c.From.Compare(ts) < 0 || (c.From == ts && c == seg.chunks[0])) && c.To.Compare(ts) >= 0 {
			return c.FirstTerm, c.LastTerm, true
		}
	}
	return 0, 0, false
}

// covers reports whether seg holds the base's [T_before, T_after] without a
// break, and whether the chain's entry at T_after belongs to the term T_after was
// read in (a base whose consistent point was rolled back is not covered).
func (seg Segment) covers(b *models.BackupRecord) bool {
	if b.TBefore == nil || b.TAfter == nil || seg.From.Compare(b.TBefore.TS) > 0 || seg.To.Compare(b.TAfter.TS) < 0 {
		return false
	}
	first, last, ok := seg.termAt(b.TAfter.TS)
	if !ok {
		return false
	}
	return b.TAfter.Term == 0 || (first <= b.TAfter.Term && b.TAfter.Term <= last)
}

// coveringSegment returns the segment of segs that covers b, if any.
func coveringSegment(segs []Segment, b *models.BackupRecord) (Segment, bool) {
	for _, s := range segs {
		if s.covers(b) {
			return s, true
		}
	}
	return Segment{}, false
}
