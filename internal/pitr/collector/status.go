package collector

import (
	"context"
	"errors"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// Window is a point-in-time window of one chain: every moment from Start to End
// can be restored. A gap splits the windows of a stream.
type Window struct {
	// ChainID is the chain the window belongs to; Open reports that it still grows.
	ChainID string `json:"chain_id"`
	Open    bool   `json:"open"`
	// Start is the consistent point (T_after) of the oldest eligible base and End
	// the end of the newest chunk, as oplog positions and wall-clock times.
	Start     pitr.Timestamp `json:"start"`
	End       pitr.Timestamp `json:"end"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	// Bases counts the eligible bases of the chain.
	Bases int `json:"bases"`
}

// ChainStatus is a chain with the span of its live chunks.
type ChainStatus struct {
	pitr.Chain
	Span pitr.ChainSpan `json:"span"`
}

// BaseStatus summarises a base backup of a stream.
type BaseStatus struct {
	ID        string              `json:"id"`
	Status    models.BackupStatus `json:"status"`
	StartedAt time.Time           `json:"started_at"`
	SizeBytes int64               `json:"size_bytes"`
	TBefore   *pitr.OpTime        `json:"t_before,omitempty"`
	TAfter    *pitr.OpTime        `json:"t_after,omitempty"`
	Pinned    bool                `json:"pinned,omitempty"`
	// ChainID is the chain whose chunks cover the base, and Eligible reports that
	// they cover [T_before, T_after].
	ChainID  string `json:"chain_id,omitempty"`
	Eligible bool   `json:"eligible"`
}

// StreamStatus is everything the dashboard shows about a stream.
type StreamStatus struct {
	Stream *pitr.Stream `json:"stream"`
	// State is the collector's stored position (nil before the first chain).
	State *pitr.State `json:"state,omitempty"`
	// Running reports a collector goroutine; Live is what it knows (nil when not
	// running).
	Running bool  `json:"running"`
	Live    *Live `json:"live,omitempty"`
	// LagSeconds and HeadroomSeconds come from the running collector.
	LagSeconds      *float64 `json:"lag_seconds,omitempty"`
	HeadroomSeconds *float64 `json:"headroom_seconds,omitempty"`
	// DurableRPOSeconds is now minus the wall-clock time of the end of the last
	// stored chunk: how much would be lost now.
	DurableRPOSeconds *float64 `json:"durable_rpo_seconds,omitempty"`
	// Windows, Chains and Bases describe what can be restored.
	Windows []Window      `json:"windows"`
	Chains  []ChainStatus `json:"chains"`
	Bases   []BaseStatus  `json:"bases"`
	// ChainBreaks counts the chains that ended in a gap, a replica set change or a
	// divergence.
	ChainBreaks int `json:"chain_breaks"`
	// Experimental is always true: the stream API may still change.
	Experimental bool `json:"experimental"`
}

// covers reports whether span covers the base's [T_before, T_after].
func covers(span pitr.ChainSpan, b *models.BackupRecord) bool {
	return span.Chunks > 0 && b.TBefore != nil && b.TAfter != nil &&
		span.From.Compare(b.TBefore.TS) <= 0 && span.To.Compare(b.TAfter.TS) >= 0
}

// eligibleBase reports whether b can start a point-in-time restore.
func eligibleBase(b *models.BackupRecord) bool {
	return b.Status == models.StatusCompleted && b.InstanceScope() && b.TBefore != nil && b.TAfter != nil
}

// Status returns the status of stream id. Expected failures: pitr.ErrNotFound.
func (s *Service) Status(ctx context.Context, id string) (*StreamStatus, error) {
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &StreamStatus{Stream: st, Windows: []Window{}, Chains: []ChainStatus{}, Bases: []BaseStatus{}, Experimental: true}
	state, err := s.cfg.Repo.LoadState(ctx, id)
	switch {
	case err == nil:
		out.State = state
		if !state.Last.TS.IsZero() {
			rpo := s.now().Sub(state.Last.TS.Time()).Seconds()
			out.DurableRPOSeconds = &rpo
		}
	case !errors.Is(err, pitr.ErrNotFound):
		return nil, err
	}
	s.mu.Lock()
	rw, running := s.workers[id]
	s.mu.Unlock()
	if running {
		out.Running = true
		live := rw.w.snapshot()
		if !live.LastTick.IsZero() {
			out.Live = &live
			lag, head := live.Lag.Seconds(), live.Headroom.Seconds()
			out.LagSeconds, out.HeadroomSeconds = &lag, &head
		}
	}
	chains, err := s.cfg.Repo.ListChains(ctx, id)
	if err != nil {
		return nil, err
	}
	spans, err := s.cfg.Repo.ChainSpans(ctx, id)
	if err != nil {
		return nil, err
	}
	byID := map[string]pitr.ChainSpan{}
	for _, sp := range spans {
		byID[sp.ChainID] = sp
	}
	for _, c := range chains {
		out.Chains = append(out.Chains, ChainStatus{Chain: *c, Span: byID[c.ChainID]})
		if c.EndReason == pitr.EndGap || c.EndReason == pitr.EndDiverged || c.EndReason == pitr.EndReplicaSetChanged {
			out.ChainBreaks++
		}
	}
	var bases []*models.BackupRecord
	if s.cfg.Bases != nil {
		if bases, err = s.cfg.Bases(ctx, id); err != nil {
			return nil, err
		}
	}
	for _, b := range bases {
		if b.Status.Deleted() {
			continue
		}
		bs := BaseStatus{ID: b.ID, Status: b.Status, StartedAt: b.StartedAt, SizeBytes: b.SizeBytes, TBefore: b.TBefore, TAfter: b.TAfter, Pinned: b.Pinned}
		for _, c := range out.Chains {
			if covers(c.Span, b) {
				bs.ChainID, bs.Eligible = c.ChainID, eligibleBase(b)
				break
			}
		}
		out.Bases = append(out.Bases, bs)
	}
	out.Windows = windows(out.Chains, out.Bases)
	return out, nil
}

// windows returns the point-in-time window of every chain with an eligible base,
// oldest first.
func windows(chains []ChainStatus, bases []BaseStatus) []Window {
	out := []Window{}
	for _, c := range chains {
		w := Window{ChainID: c.ChainID, Open: c.Open(), End: c.Span.To}
		for _, b := range bases {
			if b.ChainID != c.ChainID || !b.Eligible {
				continue
			}
			if w.Bases == 0 || b.TAfter.TS.Compare(w.Start) < 0 {
				w.Start = b.TAfter.TS
			}
			w.Bases++
		}
		if w.Bases == 0 {
			continue
		}
		w.StartTime, w.EndTime = w.Start.Time(), w.End.Time()
		out = append(out, w)
	}
	return out
}

// ListStatuses returns the status of every stream.
func (s *Service) ListStatuses(ctx context.Context) ([]*StreamStatus, error) {
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*StreamStatus, 0, len(streams))
	for _, st := range streams {
		status, err := s.Status(ctx, st.ID)
		if errors.Is(err, pitr.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

// lockStream takes the deletion lock of stream id's retention.
func lockStream(ctx context.Context, id string) (func(), error) {
	return runs.LockDeletion(ctx, runs.PITRDeletionKey(id))
}
