package cost

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// topN is how many jobs and databases a report lists.
const topN = 10

// Basis explains how the figures of a report are estimated; it is part of every
// report so no client presents them as an invoice.
const Basis = "Estimate from MongoRescue's own records: stored sizes of the archives and PITR chunks it holds, " +
	"and the bytes read back by verification (last check of each archive), restore tests and copies. " +
	"No cloud billing data is used; provider rounding, tiers, request fees and free allowances are not modelled."

// Service computes and records storage usage and cost. It is safe for concurrent
// use.
type Service struct {
	repo    Repository
	targets TargetLister
	logger  *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	last []Row
}

// Option configures a Service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithClock sets the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// New returns a Service over repo and the targets lister.
func New(repo Repository, targets TargetLister, opts ...Option) *Service {
	s := &Service{repo: repo, targets: targets, logger: slog.Default(), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// TargetTotal is the stored size of one target, for the metrics gauge.
type TargetTotal struct {
	// TargetID is the storage target.
	TargetID string
	// Bytes is the stored size.
	Bytes int64
}

// Snapshot aggregates the archives and chunks that exist now, replaces today's
// (UTC) snapshot with them, deletes the snapshots older than HistoryRetentionDays
// and returns the total of every storage target (0 for a target holding nothing).
// Running it again the same day replaces that day's rows: the day keeps its last
// values.
func (s *Service) Snapshot(ctx context.Context) ([]TargetTotal, error) {
	rows, err := s.live(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if err = s.repo.ReplaceUsageDay(ctx, now.Format(DayLayout), rows); err != nil {
		return nil, fmt.Errorf("cost: record the usage snapshot: %w", err)
	}
	cutoff := now.AddDate(0, 0, -HistoryRetentionDays).Format(DayLayout)
	if n, perr := s.repo.PruneUsage(ctx, cutoff); perr != nil {
		s.logger.Warn("could not prune the storage usage history", slog.Any("error", perr))
	} else if n > 0 {
		s.logger.Debug("pruned the storage usage history", slog.Int64("rows", n))
	}
	s.mu.Lock()
	s.last = rows
	s.mu.Unlock()

	list, err := s.targets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("cost: list storage targets: %w", err)
	}
	return totals(rows, list), nil
}

// totals sums rows per target, listing every target of list (and any other target
// the rows name).
func totals(rows []Row, list []*models.StorageTarget) []TargetTotal {
	sum := map[string]int64{}
	for _, t := range list {
		sum[t.ID] = 0
	}
	for _, r := range rows {
		sum[r.TargetID] += r.Bytes
	}
	out := make([]TargetTotal, 0, len(sum))
	for _, id := range slices.Sorted(maps.Keys(sum)) {
		out = append(out, TargetTotal{TargetID: id, Bytes: sum[id]})
	}
	return out
}

// live reads the current usage and assigns records without a target to the default
// target, where they were written.
func (s *Service) live(ctx context.Context) ([]Row, error) {
	rows, err := s.repo.LiveUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("cost: aggregate the stored sizes: %w", err)
	}
	list, err := s.targets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("cost: list storage targets: %w", err)
	}
	return assignDefault(rows, list), nil
}

// assignDefault merges the rows without a target into the default target's rows.
func assignDefault(rows []Row, list []*models.StorageTarget) []Row {
	def := ""
	for _, t := range list {
		if t.IsDefault {
			def = t.ID
		}
	}
	type key struct {
		target, job, db, stream string
		kind                    Kind
	}
	merged := map[key]*Row{}
	order := make([]key, 0, len(rows))
	for _, r := range rows {
		if r.TargetID == "" {
			if def == "" {
				continue
			}
			r.TargetID = def
		}
		k := key{r.TargetID, r.JobID, r.Database, r.StreamID, r.Kind}
		if m, ok := merged[k]; ok {
			m.Objects += r.Objects
			m.Bytes += r.Bytes
			continue
		}
		c := r
		merged[k] = &c
		order = append(order, k)
	}
	out := make([]Row, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out
}

// DayPoint is the stored size of a target at the end of one day.
type DayPoint struct {
	// Date is the UTC day, "2006-01-02".
	Date string `json:"date"`
	// Bytes is the stored size of the day's snapshot.
	Bytes int64 `json:"bytes"`
}

// EgressUsage is the estimated data read back from a target.
type EgressUsage struct {
	// Bytes is the total read over the report's range.
	Bytes int64 `json:"bytes"`
	// BySource splits Bytes by EgressSource (every source present, 0 included).
	BySource map[EgressSource]int64 `json:"by_source"`
	// Cost is the estimated egress cost in the target's currency; nil without a
	// price on the target.
	Cost *float64 `json:"cost,omitempty"`
}

// TargetUsage is the usage of one storage target.
type TargetUsage struct {
	// TargetID and Name identify the target (Name is the ID for a target that was
	// deleted while it still has history).
	TargetID string `json:"target_id"`
	Name     string `json:"name"`
	// Type is "local" or "s3"; empty for a deleted target.
	Type models.StorageType `json:"type,omitempty"`
	// StoredBytes and Objects are what the target holds now.
	StoredBytes int64 `json:"stored_bytes"`
	Objects     int64 `json:"objects"`
	// ByKind splits StoredBytes by Kind (every kind present, 0 included).
	ByKind map[Kind]int64 `json:"by_kind"`
	// GrowthBytes is the change of the stored size over the range: the latest
	// snapshot minus the earliest one in it.
	GrowthBytes int64 `json:"growth_bytes"`
	// Pricing is the price entered on the target; nil when none.
	Pricing *models.StoragePricing `json:"pricing,omitempty"`
	// MonthlyCost is the estimated cost of keeping StoredBytes for a month in the
	// currency of Pricing; nil without a price.
	MonthlyCost *float64 `json:"monthly_cost,omitempty"`
	// Egress is the estimated data read back in the range.
	Egress EgressUsage `json:"egress"`
	// History is the daily stored size over the range, oldest first.
	History []DayPoint `json:"history"`
}

// Usage is the stored size of a job or a database.
type Usage struct {
	// JobID is set for a job entry; Database for a database entry.
	JobID    string `json:"job_id,omitempty"`
	Database string `json:"database,omitempty"`
	// Bytes and Objects are the backups, copies and retained copies it has now, on
	// every target.
	Bytes   int64 `json:"bytes"`
	Objects int64 `json:"objects"`
}

// Currency is the total cost per currency (targets may price in different ones).
type Currency struct {
	// Code is the ISO 4217 code.
	Code string `json:"code"`
	// MonthlyCost and EgressCost sum the priced targets of that currency.
	MonthlyCost float64 `json:"monthly_cost"`
	EgressCost  float64 `json:"egress_cost"`
}

// Report is the answer of the usage endpoint.
type Report struct {
	// GeneratedAt is when the report was computed.
	GeneratedAt time.Time `json:"generated_at"`
	// Days is the range of the history and the egress window; From and To are its
	// first and last UTC day.
	Days int    `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
	// Targets lists every storage target, by name.
	Targets []TargetUsage `json:"targets"`
	// Totals sums the costs of the priced targets per currency.
	Totals []Currency `json:"totals"`
	// Jobs and Databases list the largest consumers now (backups, copies and
	// retained copies, not PITR), biggest first.
	Jobs      []Usage `json:"jobs"`
	Databases []Usage `json:"databases"`
	// Basis states what the estimate is and is not.
	Basis string `json:"basis"`
}

// Report computes the usage of the last days days (DefaultDays for 0). The current
// sizes are those of the latest snapshot the process took (or the live records
// before the first one); the history comes from the daily snapshots.
func (s *Service) Report(ctx context.Context, days int) (*Report, error) {
	if days == 0 {
		days = DefaultDays
	}
	if days < 1 || days > MaxDays {
		return nil, fmt.Errorf("%w: days must be between 1 and %d", ErrInvalid, MaxDays)
	}
	now := s.now().UTC()
	list, err := s.targets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("cost: list storage targets: %w", err)
	}
	s.mu.Lock()
	rows := s.last
	s.mu.Unlock()
	if rows == nil {
		if rows, err = s.live(ctx); err != nil {
			return nil, err
		}
	}
	from := now.AddDate(0, 0, -(days - 1))
	history, err := s.repo.UsageHistory(ctx, from.Format(DayLayout), now.Format(DayLayout))
	if err != nil {
		return nil, fmt.Errorf("cost: read the usage history: %w", err)
	}
	reads, err := s.repo.EgressReads(ctx, from.Truncate(24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("cost: estimate the egress: %w", err)
	}
	return build(now, days, list, rows, history, reads), nil
}

// build assembles a Report from its inputs (pure, for tests).
func build(now time.Time, days int, list []*models.StorageTarget, rows []Row, history []DayRow, reads []Read) *Report {
	r := &Report{
		GeneratedAt: now, Days: days, Basis: Basis,
		From: now.AddDate(0, 0, -(days - 1)).Format(DayLayout), To: now.Format(DayLayout),
		Targets: []TargetUsage{}, Totals: []Currency{}, Jobs: []Usage{}, Databases: []Usage{},
	}
	// Work on pointers, copy out at the end.
	ptrs := map[string]*TargetUsage{}
	get := func(id string, t *models.StorageTarget) *TargetUsage {
		if u, ok := ptrs[id]; ok {
			return u
		}
		u := &TargetUsage{TargetID: id, Name: id, ByKind: zeroKinds(), History: []DayPoint{},
			Egress: EgressUsage{BySource: zeroSources()}}
		if t != nil {
			u.Name, u.Type = t.Name, t.Type
			if t.Pricing != nil {
				p := *t.Pricing
				u.Pricing = &p
			}
		}
		ptrs[id] = u
		return u
	}
	for _, t := range list {
		get(t.ID, t)
	}

	jobs, dbs := map[string]*Usage{}, map[string]*Usage{}
	for _, row := range rows {
		u := get(row.TargetID, nil)
		u.StoredBytes += row.Bytes
		u.Objects += row.Objects
		u.ByKind[row.Kind] += row.Bytes
		if row.Kind == KindPITR {
			continue
		}
		if row.JobID != "" {
			bump(jobs, row.JobID, &Usage{JobID: row.JobID}, row)
		}
		if row.Database != "" {
			bump(dbs, row.Database, &Usage{Database: row.Database}, row)
		}
	}

	perDay := map[string]map[string]int64{}
	for _, h := range history {
		if perDay[h.TargetID] == nil {
			perDay[h.TargetID] = map[string]int64{}
		}
		perDay[h.TargetID][h.Day] += h.Bytes
		get(h.TargetID, nil)
	}
	for id, byDay := range perDay {
		u := ptrs[id]
		for _, d := range slices.Sorted(maps.Keys(byDay)) {
			u.History = append(u.History, DayPoint{Date: d, Bytes: byDay[d]})
		}
		if n := len(u.History); n > 1 {
			u.GrowthBytes = u.History[n-1].Bytes - u.History[0].Bytes
		}
	}

	since := now.AddDate(0, 0, -(days - 1)).Truncate(24 * time.Hour)
	for _, rd := range reads {
		if rd.At.Before(since) {
			continue
		}
		u := get(rd.TargetID, nil)
		u.Egress.Bytes += rd.Bytes
		u.Egress.BySource[rd.Source] += rd.Bytes
	}

	totals := map[string]*Currency{}
	for _, id := range slices.Sorted(maps.Keys(ptrs)) {
		u := ptrs[id]
		if u.Pricing != nil {
			mc, ec := u.Pricing.StorageCost(u.StoredBytes), u.Pricing.EgressCost(u.Egress.Bytes)
			u.MonthlyCost, u.Egress.Cost = &mc, &ec
			c := totals[u.Pricing.Currency]
			if c == nil {
				c = &Currency{Code: u.Pricing.Currency}
				totals[c.Code] = c
			}
			c.MonthlyCost += mc
			c.EgressCost += ec
		}
		r.Targets = append(r.Targets, *u)
	}
	slices.SortFunc(r.Targets, func(a, b TargetUsage) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.TargetID, b.TargetID))
	})
	for _, code := range slices.Sorted(maps.Keys(totals)) {
		c := *totals[code]
		c.MonthlyCost, c.EgressCost = roundTotal(c.MonthlyCost), roundTotal(c.EgressCost)
		r.Totals = append(r.Totals, c)
	}
	r.Jobs, r.Databases = top(jobs), top(dbs)
	return r
}

// roundTotal rounds a sum of costs to four decimals.
func roundTotal(v float64) float64 { return float64(int64(v*1e4+0.5)) / 1e4 }

// bump adds row to the entry of key in m, creating it from zero.
func bump(m map[string]*Usage, key string, zero *Usage, row Row) {
	u := m[key]
	if u == nil {
		u = zero
		m[key] = u
	}
	u.Bytes += row.Bytes
	u.Objects += row.Objects
}

// top returns the topN entries of m by bytes, biggest first.
func top(m map[string]*Usage) []Usage {
	out := make([]Usage, 0, len(m))
	for _, u := range m {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b Usage) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.JobID, b.JobID), cmp.Compare(a.Database, b.Database))
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

func zeroKinds() map[Kind]int64 {
	return map[Kind]int64{KindBackup: 0, KindCopy: 0, KindRetained: 0, KindPITR: 0}
}

func zeroSources() map[EgressSource]int64 {
	return map[EgressSource]int64{EgressVerification: 0, EgressCopy: 0, EgressRestoreTest: 0}
}
