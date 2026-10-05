package app

import (
	"context"

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
		case c.EndReason == pitr.EndGap, c.EndReason == pitr.EndDiverged, c.EndReason == pitr.EndReplicaSetChanged:
			info.Broken = true
		}
	}
	for _, w := range st.Windows {
		if w.ChainID == current {
			info.WindowOpen = true
		}
		start, end := w.StartTime, w.EndTime
		info.WindowStart, info.WindowEnd = &start, &end
	}
	return info
}
