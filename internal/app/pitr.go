package app

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// pitrEstimator estimates a point-in-time restore of a stream (see
// operations.Service.EstimatePITR).
type pitrEstimator func(ctx context.Context, streamID string, baseBytes, oplogBytes, oplogEntries int64) models.PITREstimate

// pitrStreams returns the PITR streams for the readiness report, from the
// collector's status (*svc is set once the collector is built), with the RTO
// estimate of each open window (*estimate is set once the operations service is
// built).
func pitrStreams(svc **collector.Service, chainTestFailed *func(ctx context.Context, streamID string) bool,
	repo pitr.Repository, estimate *pitrEstimator) readiness.StreamLister {
	return func(ctx context.Context) ([]readiness.StreamInfo, error) {
		if *svc == nil {
			return nil, nil
		}
		list, err := (*svc).ListStatuses(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]readiness.StreamInfo, 0, len(list))
		for _, st := range list {
			info := streamInfo(st)
			if *chainTestFailed != nil {
				info.ChainTestFailed = (*chainTestFailed)(ctx, info.ID)
			}
			if *estimate != nil && info.WindowOpen {
				info.RTO = pitrRTO(ctx, repo, *estimate, st)
			}
			out = append(out, info)
		}
		return out, nil
	}
}

// pitrRTO estimates the restore of the newest point of the current window of
// stream st: its newest eligible base and the live chunks after the base's
// t_before. It returns nil without an eligible base or when the chunks cannot be
// listed.
func pitrRTO(ctx context.Context, repo pitr.Repository, estimate pitrEstimator, st *collector.StreamStatus) *models.PITREstimate {
	var current string
	for _, c := range st.Chains {
		if c.Open() {
			current = c.ChainID
		}
	}
	var base *collector.BaseStatus
	for i := range st.Bases {
		b := &st.Bases[i]
		if !b.Eligible || b.ChainID != current || b.TBefore == nil || b.TAfter == nil {
			continue
		}
		if base == nil || b.TAfter.TS.Compare(base.TAfter.TS) > 0 {
			base = b
		}
	}
	if base == nil {
		return nil
	}
	chunks, err := repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: st.Stream.ID, ChainID: current, After: base.TBefore.TS,
		Status: pitr.ChunkCommitted, Live: true})
	if err != nil {
		return nil
	}
	var oplogBytes, entries int64
	for _, c := range chunks {
		oplogBytes, entries = oplogBytes+c.SizeBytes, entries+c.Entries
	}
	e := estimate(ctx, st.Stream.ID, base.SizeBytes, oplogBytes, entries)
	return &e
}

// streamInfo turns a stream's status into what the readiness report needs.
func streamInfo(st *collector.StreamStatus) readiness.StreamInfo {
	info := readiness.StreamInfo{
		ID: st.Stream.ID, ConnectionID: st.Stream.ConnectionID, ReplicaSet: st.Stream.ReplicaSet,
		Enabled: st.Stream.Enabled, Running: st.Running, DurableRPOSeconds: st.DurableRPOSeconds,
	}
	if st.State != nil {
		info.Failing = st.State.Status == pitr.CollectorFailed
		info.LagHigh = st.State.LagSince != nil
	}
	if st.Live != nil {
		info.WindowLow = st.Live.WindowLow
	}
	// Any break counts while the current chain has no window: no point after it
	// can be restored yet.
	current := ""
	for _, c := range st.Chains {
		switch {
		case c.Open():
			current = c.ChainID
			// A chunk of the current chain that failed verification breaks it
			// like a gap until a newer base covers what follows.
			if c.Corrupt > 0 {
				info.Broken = true
			}
		case c.EndReason == pitr.EndGap, c.EndReason == pitr.EndDiverged, c.EndReason == pitr.EndReplicaSetChanged:
			info.Broken = true
		}
	}
	for _, w := range st.Windows {
		if w.ChainID == current && w.Open {
			info.WindowOpen = true
		}
		start, end := w.StartTime, w.EndTime
		info.WindowStart, info.WindowEnd = &start, &end
	}
	return info
}

// oplogSession adapts *mongoconn.OplogSession to collector.Session.
type oplogSession struct{ *mongoconn.OplogSession }

// Pin selects the member a collector tick reads (see mongoconn.OplogSession.Pin).
func (s oplogSession) Pin(ctx context.Context, notBefore pitr.Timestamp) (collector.Member, error) {
	m, err := s.OplogSession.Pin(ctx, notBefore)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Primary returns the primary's majority reader.
func (s oplogSession) Primary() collector.Member { return s.OplogSession.Primary() }

// openOplogSession opens the collector session of a stream on uri.
func openOplogSession(ctx context.Context, prober *mongoconn.Prober, uri, readPreference string) (collector.Session, error) {
	s, err := prober.OpenOplogSession(ctx, uri, readPreference)
	if err != nil {
		return nil, err
	}
	return oplogSession{s}, nil
}

// databaseNames lists the database names on a server, for whole-instance
// point-in-time restores (their clone checks and clean-up).
func databaseNames(prober *mongoconn.Prober) restore.DatabaseLister {
	return func(ctx context.Context, uri string) ([]string, error) {
		dbs, err := prober.ListDatabases(ctx, uri)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(dbs))
		for _, d := range dbs {
			out = append(out, d.Name)
		}
		return out, nil
	}
}
