package readiness

import (
	"context"
	"time"
)

// PITR reasons. A stream's reasons also apply to every row of its connection.
const (
	// ReasonPITRChainBroken: a gap or a divergence ended the stream's chain and no
	// base backup covers the new one yet, so no point after the break can be
	// restored.
	ReasonPITRChainBroken = "pitr_chain_broken"
	// ReasonPITRCollectorDown: the enabled stream's collector is failing or not
	// running.
	ReasonPITRCollectorDown = "pitr_collector_down"
	// ReasonPITRLagHigh: the collector is behind the replica set by more than its
	// threshold.
	ReasonPITRLagHigh = "pitr_lag_high"
	// ReasonPITRWindowLow: the oplog headroom is below max(6h, 3 x lag).
	ReasonPITRWindowLow = "pitr_window_low"
)

// RPO sources of a row.
const (
	// RPOSourceJob: the age of the newest successful backup of a job.
	RPOSourceJob = "job"
	// RPOSourcePITR: the durable lag of the connection's PITR stream.
	RPOSourcePITR = "pitr"
)

// StreamInfo is what the report needs to know about a PITR stream (built in
// internal/app from the collector's status).
type StreamInfo struct {
	// ID, ConnectionID and ReplicaSet identify the stream.
	ID           string `json:"id"`
	ConnectionID string `json:"connection_id"`
	ReplicaSet   string `json:"replica_set"`
	// Enabled reports that the collector should run; Running that it does, and
	// Failing that its last attempt failed.
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
	Failing bool `json:"failing"`
	// DurableRPOSeconds is now minus the end of the last stored chunk; nil before
	// the first chunk.
	DurableRPOSeconds *float64 `json:"durable_rpo_seconds,omitempty"`
	// LagHigh and WindowLow report the raised lag and headroom alerts.
	LagHigh   bool `json:"lag_high"`
	WindowLow bool `json:"window_low"`
	// WindowOpen reports that the current chain has an eligible base: every point
	// from its window's start up to the durable RPO can be restored.
	WindowOpen bool `json:"window_open"`
	// WindowStart and WindowEnd bound the newest window.
	WindowStart *time.Time `json:"window_start,omitempty"`
	WindowEnd   *time.Time `json:"window_end,omitempty"`
	// Broken reports that the current chain started after a gap, a replica set
	// change or a divergence.
	Broken bool `json:"broken"`
}

// StreamRow is the readiness of one PITR stream.
type StreamRow struct {
	StreamInfo
	// ConnectionName names the connection.
	ConnectionName string `json:"connection_name,omitempty"`
	// Status and Reasons as for a Row.
	Status  Status   `json:"status"`
	Reasons []string `json:"reasons"`
}

// StreamLister returns the PITR streams.
type StreamLister func(ctx context.Context) ([]StreamInfo, error)

// streamReasons returns the fail and warn reasons of a stream.
func streamReasons(st StreamInfo) (fail, warn []string) {
	if !st.Enabled {
		return nil, nil
	}
	if !st.Running || st.Failing {
		fail = append(fail, ReasonPITRCollectorDown)
	}
	if st.Broken && !st.WindowOpen {
		fail = append(fail, ReasonPITRChainBroken)
	}
	if st.LagHigh {
		warn = append(warn, ReasonPITRLagHigh)
	}
	if st.WindowLow {
		warn = append(warn, ReasonPITRWindowLow)
	}
	return fail, warn
}

// pitrAge returns the PITR RPO of a stream: its durable lag while its window is
// open and its collector healthy; ok is false otherwise.
func pitrAge(st *StreamInfo) (float64, bool) {
	if st == nil || !st.Enabled || !st.Running || st.Failing || !st.WindowOpen || st.DurableRPOSeconds == nil {
		return 0, false
	}
	return *st.DurableRPOSeconds, true
}

// streamRows returns the readiness of every stream.
func streamRows(streams []StreamInfo, names map[string]string) []StreamRow {
	out := make([]StreamRow, 0, len(streams))
	for _, st := range streams {
		fail, warn := streamReasons(st)
		row := StreamRow{StreamInfo: st, ConnectionName: names[st.ConnectionID], Reasons: append(fail, warn...)}
		switch {
		case len(fail) > 0:
			row.Status = StatusFail
		case len(warn) > 0:
			row.Status = StatusWarn
		default:
			row.Status = StatusOK
		}
		if row.Reasons == nil {
			row.Reasons = []string{}
		}
		out = append(out, row)
	}
	return out
}
