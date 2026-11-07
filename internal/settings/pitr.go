package settings

import (
	"fmt"
	"math"
)

// DefaultBaseWindowRiskRatio is the default of PITR.BaseWindowRiskRatio.
const DefaultBaseWindowRiskRatio = 0.5

// PITR configures point-in-time recovery warnings.
type PITR struct {
	// BaseWindowRiskRatio is the share of the oplog window the last successful base
	// backup may take before the stream is reported at risk (event
	// pitr.base_window_risk, readiness reason pitr_base_window_risk): a base that
	// outlasts the window fails, and the recovery window stops moving. Greater than
	// 0 and at most 1; default 0.5.
	BaseWindowRiskRatio float64 `json:"base_window_risk_ratio"`
}

// defaultPITR returns the PITR settings of a fresh installation.
func defaultPITR() PITR {
	return PITR{BaseWindowRiskRatio: DefaultBaseWindowRiskRatio}
}

// PITRPatch updates PITR; see PITR for the fields.
type PITRPatch struct {
	BaseWindowRiskRatio *float64 `json:"base_window_risk_ratio,omitempty"`
}

// apply sets the fields of p that are present on s. A nil p changes nothing.
func (p *PITRPatch) apply(s *PITR) {
	if p == nil {
		return
	}
	setIf(&s.BaseWindowRiskRatio, p.BaseWindowRiskRatio)
}

// validatePITR checks s.
func validatePITR(s *PITR) error {
	r := s.BaseWindowRiskRatio
	if math.IsNaN(r) || r <= 0 || r > 1 {
		return fmt.Errorf("%w: pitr.base_window_risk_ratio must be greater than 0 and at most 1", ErrInvalid)
	}
	return nil
}
