package readiness

import (
	"cmp"
	"context"
	"slices"
	"time"

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
	// ReasonDRDrillStale: no disaster recovery drill (a restore test reading a copy
	// target, models.RestoreTestPolicy.SourceTargetID) of the database passed in the
	// last Config.DRDrillMaxAge.
	ReasonDRDrillStale = "dr_drill_stale"
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
	// LastDrill is the newest disaster recovery drill of the database, LastGoodDrill
	// the newest one that passed; nil without one.
	LastDrill     *RestoreTestRef `json:"last_drill,omitempty"`
	LastGoodDrill *RestoreTestRef `json:"last_good_drill,omitempty"`
	// DrillStale reports that no drill passed within the drill age limit
	// (ReasonDRDrillStale).
	DrillStale bool `json:"drill_stale"`
}

// drAcc collects the disaster recovery posture of a row.
type drAcc struct {
	status DRStatus
	// cross holds the copy targets in another region than their primary.
	cross map[string]bool
	jobs  map[string]bool
	// maxAge is Config.DRDrillMaxAge.
	maxAge time.Duration
}

// drillLister lists the newest disaster recovery drills (implemented by
// *store.SQLiteStore).
type drillLister interface {
	LatestDRDrillsAll(ctx context.Context) (map[string][]*models.RestoreTestResult, error)
}

// drills returns the newest drills of every job (see drillLister); a store without
// them or a failed listing gives none.
func (s *Service) drills(ctx context.Context) map[string][]*models.RestoreTestResult {
	l, ok := s.cfg.Store.(drillLister)
	if !ok {
		return map[string][]*models.RestoreTestResult{}
	}
	out, err := l.LatestDRDrillsAll(ctx)
	if err != nil {
		s.logger.Warn("readiness: cannot list the disaster recovery drills", logsafe.Error(err))
		return map[string][]*models.RestoreTestResult{}
	}
	return out
}

// addDrills adds the drills (newest first) of p's job for p's database to acc's
// disaster recovery posture, if it has one.
func addDrills(acc *rowAcc, p point, list []*models.RestoreTestResult) {
	d := acc.dr
	if d == nil {
		return
	}
	for _, t := range list {
		db := t.Database
		if db == "" && !p.job.MultiDatabase() {
			db = p.job.Database
		}
		if db != p.database {
			continue
		}
		ref := testRef(p.job.ID, t)
		if cur := d.status.LastDrill; cur == nil || ref.At.After(cur.At) {
			d.status.LastDrill = ref
		}
		if t.Status == models.RestoreTestOK {
			if cur := d.status.LastGoodDrill; cur == nil || ref.At.After(cur.At) {
				d.status.LastGoodDrill = ref
			}
			break
		}
	}
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
func finishDR(acc *rowAcc, now time.Time) []string {
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
	maxAge := cmp.Or(d.maxAge, DefaultDRDrillMaxAge)
	if g := st.LastGoodDrill; g == nil || now.Sub(g.At) > maxAge {
		st.DrillStale = true
		warn = append(warn, ReasonDRDrillStale)
	}
	acc.row.DR = &st
	return warn
}
