package operations

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/scheduler"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// RetentionHold is a lowering of a job's retention that a job edit held back: the job
// is stored with its current retention, and the lower values take effect after the
// grace period (ApplyRetentionHold) or, with the two-person rule, after a second
// administrator approved them. It covers retention_days, retention_count, the keep_*
// buckets and the copy_retention overrides: a value counts as lowered when it keeps
// less (see lowerRetention).
type RetentionHold struct {
	// JobID is the job.
	JobID string
	// Days, Count, Daily, Weekly, Monthly and Yearly are the requested lower values
	// of retention_days, retention_count and the keep_* buckets (0 turns the rule
	// off); nil when that value was not lowered.
	Days, Count, Daily, Weekly, Monthly, Yearly *int
	// Copies holds, by copy target ID, the requested copy retention override of
	// every target whose override was lowered; nil removes the override.
	Copies map[string]*models.RetentionPolicy
	// From describes the retention in force.
	From string
}

// empty reports whether the hold holds back nothing.
func (h *RetentionHold) empty() bool {
	return h.Days == nil && h.Count == nil && h.Daily == nil && h.Weekly == nil && h.Monthly == nil && h.Yearly == nil && len(h.Copies) == 0
}

// shorter reports whether retention value v (0 keeps forever) keeps less than old.
func shorter(v, old int) bool {
	return v > 0 && (old == 0 || v < old)
}

// lowerBucket reports whether bucket value v is lower than old: any decrease counts,
// including a rule turned off (N to 0). limit says the policy went from keeping
// everything to a limited one, which makes a newly set bucket a lowering too.
func lowerBucket(v, old int, limit bool) bool {
	return v < old || (limit && v > 0)
}

// retentionValues holds the lowered values of a policy: a pointer per value, nil when
// the value was not lowered.
type retentionValues struct {
	days, count, daily, weekly, monthly, yearly *int
}

func (v retentionValues) any() bool {
	return v.days != nil || v.count != nil || v.daily != nil || v.weekly != nil || v.monthly != nil || v.yearly != nil
}

// lowerRetention returns the values of next that keep less than those of cur, the
// policy in force:
//   - retention_days and retention_count: a shorter limit, or a limit where there was
//     none (0 keeps forever);
//   - keep_daily, keep_weekly, keep_monthly and keep_yearly: any decrease, including
//     N to 0, and any bucket set while no rule was on;
//   - turning the age and count rule off while buckets remain, which leaves only the
//     buckets to keep backups (the held values are 0).
//
// An increase is never returned; it applies at once.
func lowerRetention(cur, next models.RetentionPolicy) retentionValues {
	var v retentionValues
	limit := !cur.Active() && next.Active()
	set := func(dst **int, n int, lowered bool) {
		if lowered {
			*dst = &n
		}
	}
	set(&v.days, next.RetentionDays, shorter(next.RetentionDays, cur.RetentionDays))
	set(&v.count, next.RetentionCount, shorter(next.RetentionCount, cur.RetentionCount))
	if cur.RetentionDays+cur.RetentionCount > 0 && next.RetentionDays+next.RetentionCount == 0 && next.GFS() {
		set(&v.days, 0, cur.RetentionDays > 0)
		set(&v.count, 0, cur.RetentionCount > 0)
	}
	set(&v.daily, next.KeepDaily, lowerBucket(next.KeepDaily, cur.KeepDaily, limit))
	set(&v.weekly, next.KeepWeekly, lowerBucket(next.KeepWeekly, cur.KeepWeekly, limit))
	set(&v.monthly, next.KeepMonthly, lowerBucket(next.KeepMonthly, cur.KeepMonthly, limit))
	set(&v.yearly, next.KeepYearly, lowerBucket(next.KeepYearly, cur.KeepYearly, limit))
	return v
}

// keepCurrent resets, in p, every value v names to its value in cur.
func (v retentionValues) keepCurrent(p *models.RetentionPolicy, cur models.RetentionPolicy) {
	if v.days != nil {
		p.RetentionDays = cur.RetentionDays
	}
	if v.count != nil {
		p.RetentionCount = cur.RetentionCount
	}
	if v.daily != nil {
		p.KeepDaily = cur.KeepDaily
	}
	if v.weekly != nil {
		p.KeepWeekly = cur.KeepWeekly
	}
	if v.monthly != nil {
		p.KeepMonthly = cur.KeepMonthly
	}
	if v.yearly != nil {
		p.KeepYearly = cur.KeepYearly
	}
}

// RetentionChanged reports whether the retention of job differs from that of
// existing in any value a hold covers (the job's own policy and its copy retention
// overrides).
func RetentionChanged(existing, job *models.Job) bool {
	return job.RetentionPolicy() != existing.RetentionPolicy() || !maps.Equal(job.CopyRetention, existing.CopyRetention)
}

// HoldRetention compares the retention of job, about to be stored, with that of
// existing (nil for a new job). A lowered value (see RetentionHold) is reset to the
// current one in job and returned in the hold, to be applied with ApplyRetentionHold
// once job is stored. A new job whose ID already names backups counts as keeping them
// forever, so it cannot lower their retention either. A copy retention override that
// is lowered or removed is held as a whole; one for a target the job no longer copies
// to is dropped with the target. It returns nil when nothing was lowered.
func (s *Service) HoldRetention(ctx context.Context, existing, job *models.Job) (*RetentionHold, error) {
	baseline := existing
	if baseline == nil {
		page, err := s.store.QueryBackupRecords(ctx, store.BackupFilter{JobID: job.ID, Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("check the backups of job %s: %w", job.ID, err)
		}
		if page.Total == 0 {
			return nil, nil
		}
		baseline = &models.Job{}
	}
	h := &RetentionHold{JobID: job.ID, From: retentionSummary(baseline.RetentionPolicy())}
	cur, next := baseline.RetentionPolicy(), job.RetentionPolicy()
	origNext := next
	v := lowerRetention(cur, next)
	h.Days, h.Count, h.Daily, h.Weekly, h.Monthly, h.Yearly = v.days, v.count, v.daily, v.weekly, v.monthly, v.yearly
	v.keepCurrent(&next, cur)
	job.RetentionDays, job.RetentionCount = next.RetentionDays, next.RetentionCount
	job.KeepDaily, job.KeepWeekly, job.KeepMonthly, job.KeepYearly = next.KeepDaily, next.KeepWeekly, next.KeepMonthly, next.KeepYearly
	submitted := maps.Clone(job.CopyRetention)
	for id, old := range baseline.CopyRetention {
		if !slices.Contains(job.CopyTargets, id) {
			continue
		}
		n, ok := job.CopyRetention[id]
		if ok && !lowerRetention(old, n).any() {
			continue
		}
		if h.Copies == nil {
			h.Copies = map[string]*models.RetentionPolicy{}
		}
		var held *models.RetentionPolicy
		if ok {
			held = &n
		}
		h.Copies[id] = held
		job.CopyRetention = maps.Clone(job.CopyRetention)
		if job.CopyRetention == nil {
			job.CopyRetention = map[string]models.RetentionPolicy{}
		}
		job.CopyRetention[id] = old
	}
	if h.empty() {
		return nil, nil
	}
	// A lowering already pending for the job keeps the values this edit left alone.
	if existing != nil {
		if earlier := s.pendingRetention(ctx, job.ID); earlier != nil && earlier.JobCreatedAt != nil && earlier.JobCreatedAt.Equal(existing.CreatedAt) {
			h = h.merge(holdOfPending(earlier).unaltered(cur, origNext, submitted, existing.CopyRetention))
		}
	}
	return h, nil
}

// retentionSummary describes a policy ("30 days and 5 backups, keep_daily 7").
func retentionSummary(p models.RetentionPolicy) string {
	out := fmt.Sprintf("%d days and %d backups", p.RetentionDays, p.RetentionCount)
	var parts []string
	for _, b := range []struct {
		name string
		n    int
	}{{"keep_daily", p.KeepDaily}, {"keep_weekly", p.KeepWeekly}, {"keep_monthly", p.KeepMonthly}, {"keep_yearly", p.KeepYearly}} {
		if b.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", b.name, b.n))
		}
	}
	if len(parts) > 0 {
		out += ", " + strings.Join(parts, ", ")
	}
	return out
}

// text describes the lowered values of the hold ("30 days", "keep_monthly 0", a copy
// override).
func (h *RetentionHold) text() string {
	var parts []string
	if h.Days != nil {
		parts = append(parts, fmt.Sprintf("%d days", *h.Days))
	}
	if h.Count != nil {
		parts = append(parts, fmt.Sprintf("%d backups", *h.Count))
	}
	for _, f := range []struct {
		name string
		v    *int
	}{{"keep_daily", h.Daily}, {"keep_weekly", h.Weekly}, {"keep_monthly", h.Monthly}, {"keep_yearly", h.Yearly}} {
		if f.v != nil {
			parts = append(parts, fmt.Sprintf("%s %d", f.name, *f.v))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(h.Copies)) {
		if p := h.Copies[id]; p == nil {
			parts = append(parts, "no copy retention override on "+id)
		} else {
			parts = append(parts, "copy retention of "+id+": "+retentionSummary(*p))
		}
	}
	return strings.Join(parts, ", ")
}

// holdOfApproval returns the retention hold an approval request of
// ApprovalShortenRetention carries.
func holdOfApproval(a *models.Approval) *RetentionHold {
	return &RetentionHold{JobID: a.Subject, Days: a.RetentionDays, Count: a.RetentionCount,
		Daily: a.KeepDaily, Weekly: a.KeepWeekly, Monthly: a.KeepMonthly, Yearly: a.KeepYearly, Copies: a.CopyRetention}
}

// holdOfPending returns the retention hold a pending change carries.
func holdOfPending(c *models.PendingChange) *RetentionHold {
	return &RetentionHold{JobID: c.JobID, Days: c.RetentionDays, Count: c.RetentionCount,
		Daily: c.KeepDaily, Weekly: c.KeepWeekly, Monthly: c.KeepMonthly, Yearly: c.KeepYearly, Copies: c.CopyRetention}
}

// merge returns h with the values of earlier that h does not set: a second lowering
// of the same job adds to a pending one instead of replacing it.
func (h *RetentionHold) merge(earlier *RetentionHold) *RetentionHold {
	out := *h
	pick := func(dst **int, old *int) {
		if *dst == nil {
			*dst = old
		}
	}
	pick(&out.Days, earlier.Days)
	pick(&out.Count, earlier.Count)
	pick(&out.Daily, earlier.Daily)
	pick(&out.Weekly, earlier.Weekly)
	pick(&out.Monthly, earlier.Monthly)
	pick(&out.Yearly, earlier.Yearly)
	if len(earlier.Copies) > 0 {
		out.Copies = maps.Clone(earlier.Copies)
		maps.Copy(out.Copies, h.Copies)
	}
	return &out
}

// unaltered returns h without the values an edit changed: next is the policy the edit
// submitted, cur the stored one (a value the edit left at its stored value keeps its
// held lowering, one it raised drops it), and submitted and stored the copy
// retention overrides of the edit and of the stored job.
func (h *RetentionHold) unaltered(cur, next models.RetentionPolicy, submitted, stored map[string]models.RetentionPolicy) *RetentionHold {
	keep := func(v *int, same bool) *int {
		if same {
			return v
		}
		return nil
	}
	out := &RetentionHold{JobID: h.JobID, From: h.From,
		Days: keep(h.Days, next.RetentionDays == cur.RetentionDays), Count: keep(h.Count, next.RetentionCount == cur.RetentionCount),
		Daily: keep(h.Daily, next.KeepDaily == cur.KeepDaily), Weekly: keep(h.Weekly, next.KeepWeekly == cur.KeepWeekly),
		Monthly: keep(h.Monthly, next.KeepMonthly == cur.KeepMonthly), Yearly: keep(h.Yearly, next.KeepYearly == cur.KeepYearly)}
	for id, p := range h.Copies {
		s, inSubmitted := submitted[id]
		o, inStored := stored[id]
		if inSubmitted != inStored || s != o {
			continue
		}
		if out.Copies == nil {
			out.Copies = map[string]*models.RetentionPolicy{}
		}
		out.Copies[id] = p
	}
	return out
}

// applyTo sets the held values in job; a copy override of a target the job no longer
// copies to is skipped.
func (h *RetentionHold) applyTo(job *models.Job) {
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	set(&job.RetentionDays, h.Days)
	set(&job.RetentionCount, h.Count)
	set(&job.KeepDaily, h.Daily)
	set(&job.KeepWeekly, h.Weekly)
	set(&job.KeepMonthly, h.Monthly)
	set(&job.KeepYearly, h.Yearly)
	if len(h.Copies) == 0 {
		return
	}
	copies := maps.Clone(job.CopyRetention)
	if copies == nil {
		copies = map[string]models.RetentionPolicy{}
	}
	for id, p := range h.Copies {
		switch {
		case p == nil:
			delete(copies, id)
		case slices.Contains(job.CopyTargets, id):
			copies[id] = *p
		}
	}
	if len(copies) == 0 {
		copies = nil
	}
	job.CopyRetention = copies
}

// ApplyRetentionHold schedules the lowering h after the grace period or, with the
// two-person rule, asks a second administrator first. retentionChanged reports that
// the stored job's retention changed otherwise (raised): that drops a pending
// lowering of the job when there is no new one.
func (s *Service) ApplyRetentionHold(ctx context.Context, jobID string, h *RetentionHold, retentionChanged bool) (*JobProtection, error) {
	out := &JobProtection{}
	if retentionChanged {
		// A raise cancels the pending lowering; the values of it the edit left alone
		// are in h again.
		if st, ok := s.cfg.Store.(pendingStore); ok {
			if _, err := st.DeletePendingChangesOf(ctx, models.PendingRetention, jobID); err != nil {
				return nil, fmt.Errorf("cancel the pending retention change: %w", err)
			}
		}
	}
	if h == nil {
		out.PendingRetention = s.pendingRetention(ctx, jobID)
		return out, nil
	}
	if s.needsApproval(ctx) {
		var bound *time.Time
		if job, err := s.store.GetJob(ctx, jobID); err == nil {
			created := job.CreatedAt
			bound = &created
		}
		a, err := s.storeApproval(ctx, &models.Approval{Action: models.ApprovalShortenRetention, Subject: jobID,
			RetentionDays: h.Days, RetentionCount: h.Count,
			KeepDaily: h.Daily, KeepWeekly: h.Weekly, KeepMonthly: h.Monthly, KeepYearly: h.Yearly, CopyRetention: h.Copies,
			SubjectCreatedAt: bound,
			Summary:          fmt.Sprintf("lower the retention of job %s from %s to %s", jobID, h.From, h.text())})
		if err != nil {
			return nil, err
		}
		out.Approval = a
		out.PendingRetention = s.pendingRetention(ctx, jobID)
		return out, nil
	}
	c, err := s.scheduleRetention(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	out.PendingRetention = c
	return out, nil
}

// PendingRetentionImpact is what a pending retention lowering would release: the
// backups the lowered policy would delete that the current one keeps, and the copies
// the lowered copy retention would stop keeping.
type PendingRetentionImpact struct {
	// ChangeID is the pending change that waits for the grace period.
	ChangeID string `json:"change_id,omitempty"`
	// ApprovalID is the approval request that waits for a second administrator.
	ApprovalID string `json:"approval_id,omitempty"`
	// RetentionDays, RetentionCount and the keep_* values are the policy that would
	// be in force once the change applies.
	RetentionDays  int `json:"retention_days"`
	RetentionCount int `json:"retention_count"`
	KeepDaily      int `json:"keep_daily"`
	KeepWeekly     int `json:"keep_weekly"`
	KeepMonthly    int `json:"keep_monthly"`
	KeepYearly     int `json:"keep_yearly"`
	// Release lists the backups the change would release for deletion, oldest first.
	Release []ReleasedBackup `json:"release"`
	// Copies lists, per copy target, the copies the change would release.
	Copies []ReleasedCopies `json:"copies"`
}

// ReleasedBackup is a backup a retention lowering would delete.
type ReleasedBackup struct {
	// BackupID and Database identify the backup, StartedAt is its start time.
	BackupID  string    `json:"backup_id"`
	Database  string    `json:"database"`
	StartedAt time.Time `json:"started_at"`
	// Reason is the retention rule that would delete it ("max_age", "max_count" or
	// "gfs"), Detail says why.
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// ReleasedCopies lists the backups whose copy on one target a lowered or removed
// copy retention override would stop keeping.
type ReleasedCopies struct {
	// TargetID and TargetName identify the copy target.
	TargetID   string `json:"target_id"`
	TargetName string `json:"target_name,omitempty"`
	// Backups are the backups whose copy on the target is released.
	Backups []string `json:"backups"`
}

// pendingImpact previews the lowering h of job: what it would release if it applied
// now.
func (s *Service) pendingImpact(ctx context.Context, job *models.Job, h *RetentionHold, now time.Time, records []*models.BackupRecord, target string) *PendingRetentionImpact {
	after := job.Clone()
	h.applyTo(after)
	loc := job.RetentionLocation()
	current := scheduler.PlanRetentionPolicy(now, job.RetentionPolicy(), loc, scheduler.JobRetentionHistory(job, target, records))
	lowered := scheduler.PlanRetentionPolicy(now, after.RetentionPolicy(), loc, scheduler.JobRetentionHistory(after, target, records))
	deleting := map[string]bool{}
	for _, d := range current.Delete {
		deleting[d.Backup.ID] = true
	}
	p := after.RetentionPolicy()
	out := &PendingRetentionImpact{
		RetentionDays: p.RetentionDays, RetentionCount: p.RetentionCount,
		KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly, KeepMonthly: p.KeepMonthly, KeepYearly: p.KeepYearly,
		Release: []ReleasedBackup{}, Copies: []ReleasedCopies{},
	}
	atRisk := maps.Clone(deleting)
	for _, d := range lowered.Delete {
		atRisk[d.Backup.ID] = true
		if !deleting[d.Backup.ID] {
			out.Release = append(out.Release, ReleasedBackup{BackupID: d.Backup.ID, Database: d.Backup.Database, StartedAt: d.Backup.StartedAt, Reason: string(d.Reason), Detail: d.Detail})
		}
	}
	keepsNow := scheduler.PlanCopyRetention(now, job, records)
	keepsAfter := scheduler.PlanCopyRetention(now, after, records)
	for _, id := range slices.Sorted(maps.Keys(keepsNow)) {
		var released []string
		for _, r := range records {
			if _, kept := keepsNow[id][r.ID]; !kept {
				continue
			}
			if _, still := keepsAfter[id][r.ID]; still {
				continue
			}
			// A copy only needs its override when the primary is (or will be) gone.
			if r.Status == models.StatusDeleted || atRisk[r.ID] {
				released = append(released, r.ID)
			}
		}
		if len(released) == 0 {
			continue
		}
		slices.Sort(released)
		c := ReleasedCopies{TargetID: id, Backups: released}
		if s.cfg.Targets != nil {
			if t, err := s.cfg.Targets.Resolve(ctx, id); err == nil {
				c.TargetName = t.Name
			}
		}
		out.Copies = append(out.Copies, c)
	}
	return out
}

// pendingRetentionImpact previews the retention lowering of job that waits for the
// grace period (a pending change) or for a second administrator (an open approval
// request), nil when there is none. It never fails the preview.
func (s *Service) pendingRetentionImpact(ctx context.Context, job *models.Job, now time.Time, records []*models.BackupRecord, target string) *PendingRetentionImpact {
	if c := s.pendingRetention(ctx, job.ID); c != nil {
		out := s.pendingImpact(ctx, job, holdOfPending(c), now, records, target)
		out.ChangeID = c.ID
		return out
	}
	st, err := s.approvals()
	if err != nil {
		return nil
	}
	list, err := st.ListApprovals(ctx, models.ApprovalPending, store.MaxApprovalList)
	if err != nil {
		return nil
	}
	for _, a := range list {
		if a.Action == models.ApprovalShortenRetention && a.Subject == job.ID && a.Open(now) {
			out := s.pendingImpact(ctx, job, holdOfApproval(a), now, records, target)
			out.ApprovalID = a.ID
			return out
		}
	}
	return nil
}
