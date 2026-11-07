package pitr

import "time"

// DefaultBaseWindowRiskRatio is the default of the setting
// pitr.base_window_risk_ratio: a base backup that took more than this share of
// the oplog window is at risk of outlasting it.
const DefaultBaseWindowRiskRatio = 0.5

// BaseHistory is how many of a stream's newest successful base backups the
// expected base duration looks at.
const BaseHistory = 5

// BaseWindow compares how long a stream's base backups take with the oplog window
// of its replica set. A base is taken with mongodump --oplog, which fails when
// the oplog rolls past the dump's start before the dump ends; the new base is
// then never eligible and the point-in-time window stops moving forward.
type BaseWindow struct {
	// LastBaseSeconds is the duration of the newest successful base backup.
	LastBaseSeconds float64 `json:"last_base_seconds"`
	// ExpectedBaseSeconds is the longest of the newest BaseHistory successful
	// bases: what the next base may take.
	ExpectedBaseSeconds float64 `json:"expected_base_seconds"`
	// OplogWindowSeconds is the span of the oplog (newest minus oldest entry) now.
	OplogWindowSeconds float64 `json:"oplog_window_seconds"`
	// Ratio is LastBaseSeconds over OplogWindowSeconds, ExpectedRatio the same for
	// ExpectedBaseSeconds, and Threshold the ratio above which the stream is at
	// risk.
	Ratio         float64 `json:"ratio"`
	ExpectedRatio float64 `json:"expected_ratio"`
	Threshold     float64 `json:"threshold"`
	// AtRisk reports Ratio above Threshold.
	AtRisk bool `json:"at_risk"`
}

// NewBaseWindow compares a last and an expected base duration with an oplog
// window. It returns nil without a base duration or with an empty window, where
// no ratio exists. A threshold of 0 or less means DefaultBaseWindowRiskRatio.
func NewBaseWindow(last, expected, window time.Duration, threshold float64) *BaseWindow {
	if last <= 0 || window <= 0 {
		return nil
	}
	if threshold <= 0 {
		threshold = DefaultBaseWindowRiskRatio
	}
	b := &BaseWindow{
		LastBaseSeconds: last.Seconds(), ExpectedBaseSeconds: expected.Seconds(), OplogWindowSeconds: window.Seconds(),
		Threshold: threshold,
	}
	b.Ratio = b.LastBaseSeconds / b.OplogWindowSeconds
	b.ExpectedRatio = b.ExpectedBaseSeconds / b.OplogWindowSeconds
	b.AtRisk = b.Ratio > threshold
	return b
}
