package readiness

import (
	"context"
	"slices"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// Disaster recovery reasons (warnings) of a row whose jobs copy their backups to
// other storage targets.
const (
	// ReasonDRSameRegion: no copy target of the row's jobs is in a known region
	// other than its primary target's (models.CrossRegion).
	ReasonDRSameRegion = "dr_same_region"
	// ReasonDRSameCredentials: every copy target of the row's jobs is reachable with
	// the credentials or account of its primary target (models.SameCredentials).
	ReasonDRSameCredentials = "dr_same_credentials"
)

// DR levels of a row (DRStatus.Level).
const (
	// DRLevelCrossRegion: a copy target in another region than the primary.
	DRLevelCrossRegion = "cross_region"
	// DRLevelSameRegion: copies, but none in another region.
	DRLevelSameRegion = "same_region"
)

// DRStatus is the disaster recovery posture of a row whose jobs copy their backups
// to other storage targets.
type DRStatus struct {
	// Level is DRLevelCrossRegion or DRLevelSameRegion.
	Level string `json:"level"`
	// CrossRegion reports a copy target in a known region other than its primary's.
	CrossRegion bool `json:"cross_region"`
	// SeparateCredentials reports a copy target that is not detectably reachable
	// with its primary's credentials or account.
	SeparateCredentials bool `json:"separate_credentials"`
	// LockedCopy reports a cross-region copy target with S3 Object Lock.
	LockedCopy bool `json:"locked_copy"`
	// PrimaryRegions and CopyRegions list the known regions of the primary and
	// copy targets, sorted ("" regions are left out).
	PrimaryRegions []string `json:"primary_regions,omitempty"`
	CopyRegions    []string `json:"copy_regions,omitempty"`
}

// drAcc collects the disaster recovery posture of a row.
type drAcc struct {
	status DRStatus
	// cross holds the copy targets in another region than their primary.
	cross map[string]bool
	jobs  map[string]bool
}

// targetsByID lists the storage targets by ID; nil without Config.Targets or when
// the listing fails.
func (s *Service) targetsByID(ctx context.Context) map[string]*models.StorageTarget {
	if s.cfg.Targets == nil {
		return nil
	}
	list, err := s.cfg.Targets(ctx)
	if err != nil {
		s.logger.Warn("readiness: cannot list the storage targets", logsafe.Error(err))
		return nil
	}
	out := make(map[string]*models.StorageTarget, len(list))
	for _, t := range list {
		out[t.ID] = t
	}
	return out
}

// addDR adds the copy targets of job to acc's disaster recovery posture; targets
// maps the storage targets by ID (nil skips the check).
func addDR(acc *rowAcc, job *models.Job, targets map[string]*models.StorageTarget) {
	if targets == nil || len(job.CopyTargets) == 0 {
		return
	}
	d := acc.dr
	if d == nil {
		d = &drAcc{cross: map[string]bool{}, jobs: map[string]bool{}}
		acc.dr = d
	}
	if d.jobs[job.ID] {
		return
	}
	d.jobs[job.ID] = true
	primary := targets[job.StorageTargetID]
	if r := primary.DRRegion(); r != "" && !slices.Contains(d.status.PrimaryRegions, r) {
		d.status.PrimaryRegions = append(d.status.PrimaryRegions, r)
	}
	for _, id := range job.CopyTargets {
		c := targets[id]
		if c == nil {
			continue
		}
		if r := c.DRRegion(); r != "" && !slices.Contains(d.status.CopyRegions, r) {
			d.status.CopyRegions = append(d.status.CopyRegions, r)
		}
		if primary == nil {
			continue
		}
		if models.CrossRegion(primary, c) {
			d.status.CrossRegion = true
			d.cross[c.ID] = true
			if c.ObjectLocked() {
				d.status.LockedCopy = true
			}
		}
		if !models.SameCredentials(primary, c) {
			d.status.SeparateCredentials = true
		}
	}
}

// finishDR sets the row's DR status and returns its warnings.
func finishDR(acc *rowAcc) []string {
	d := acc.dr
	if d == nil {
		return nil
	}
	st := d.status
	slices.Sort(st.PrimaryRegions)
	slices.Sort(st.CopyRegions)
	var warn []string
	st.Level = DRLevelSameRegion
	if st.CrossRegion {
		st.Level = DRLevelCrossRegion
	} else {
		warn = append(warn, ReasonDRSameRegion)
	}
	if !st.SeparateCredentials {
		warn = append(warn, ReasonDRSameCredentials)
	}
	acc.row.DR = &st
	return warn
}
