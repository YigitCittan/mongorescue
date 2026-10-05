package app

import (
	"context"

	"github.com/yigitcittan/mongorescue/internal/mongoconn"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
	"github.com/yigitcittan/mongorescue/internal/readiness"
)

// pitrStreams returns the PITR streams for the readiness report, from the
// collector's status (*svc is set once the collector is built).
func pitrStreams(svc **collector.Service) readiness.StreamLister {
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
			out = append(out, streamInfo(st))
		}
		return out, nil
	}
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
