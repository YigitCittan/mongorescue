package cost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// fakeRepo is an in-memory Repository.
type fakeRepo struct {
	live    []Row
	days    map[string][]Row
	reads   []Read
	pruned  string
	failing bool
}

func (f *fakeRepo) LiveUsage(context.Context) ([]Row, error) {
	if f.failing {
		return nil, errors.New("boom")
	}
	return append([]Row(nil), f.live...), nil
}

func (f *fakeRepo) ReplaceUsageDay(_ context.Context, day string, rows []Row) error {
	if f.days == nil {
		f.days = map[string][]Row{}
	}
	f.days[day] = append([]Row(nil), rows...)
	return nil
}

func (f *fakeRepo) UsageHistory(_ context.Context, from, to string) ([]DayRow, error) {
	var out []DayRow
	for day, rows := range f.days {
		if day < from || day > to {
			continue
		}
		for _, r := range rows {
			out = append(out, DayRow{Row: r, Day: day})
		}
	}
	return out, nil
}

func (f *fakeRepo) PruneUsage(_ context.Context, before string) (int64, error) {
	f.pruned = before
	return 0, nil
}

func (f *fakeRepo) EgressReads(context.Context, time.Time) ([]Read, error) { return f.reads, nil }

type fakeTargets []*models.StorageTarget

func (f fakeTargets) List(context.Context) ([]*models.StorageTarget, error) { return f, nil }

const gib = models.BytesPerGiB

func target(id, name string, def bool, p *models.StoragePricing) *models.StorageTarget {
	return &models.StorageTarget{ID: id, Name: name, Type: models.StorageS3, IsDefault: def, Pricing: p}
}

func TestSnapshotIsIdempotentPerDay(t *testing.T) {
	now := time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC)
	repo := &fakeRepo{live: []Row{{TargetID: "a", Kind: KindBackup, JobID: "j", Database: "d", Objects: 1, Bytes: 100}}}
	svc := New(repo, fakeTargets{target("a", "A", true, nil), target("b", "B", false, nil)}, WithClock(func() time.Time { return now }))

	totals, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[0] != (TargetTotal{"a", 100}) || totals[1] != (TargetTotal{"b", 0}) {
		t.Errorf("totals = %+v; want every target, the empty one with 0", totals)
	}

	// The same day again, with the data grown: the day holds the last values.
	repo.live[0].Bytes = 150
	now = now.Add(5 * time.Hour)
	for range 2 {
		if _, err = svc.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.days) != 1 || len(repo.days["2026-03-05"]) != 1 || repo.days["2026-03-05"][0].Bytes != 150 {
		t.Errorf("days = %+v; want one day holding 150 bytes", repo.days)
	}
	// The next UTC day is a new snapshot, and old history is pruned.
	now = now.Add(24 * time.Hour)
	if _, err = svc.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.days) != 2 {
		t.Errorf("days = %+v; want two days", repo.days)
	}
	if want := now.AddDate(0, 0, -HistoryRetentionDays).Format(DayLayout); repo.pruned != want {
		t.Errorf("pruned before %q, want %q", repo.pruned, want)
	}
}

func TestSnapshotAssignsRecordsWithoutTargetToTheDefault(t *testing.T) {
	repo := &fakeRepo{live: []Row{
		{TargetID: "", Kind: KindBackup, Database: "d", Objects: 1, Bytes: 10},
		{TargetID: "a", Kind: KindBackup, Database: "d", Objects: 1, Bytes: 5},
	}}
	svc := New(repo, fakeTargets{target("a", "A", true, nil)})
	totals, err := svc.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].Bytes != 15 {
		t.Errorf("totals = %+v; want 15 bytes on the default target", totals)
	}
	if rows := repo.days[time.Now().UTC().Format(DayLayout)]; len(rows) != 1 || rows[0].Objects != 2 {
		t.Errorf("rows = %+v; want the two rows merged", rows)
	}
}

func TestSnapshotReportsRepositoryFailures(t *testing.T) {
	svc := New(&fakeRepo{failing: true}, fakeTargets{})
	if _, err := svc.Snapshot(context.Background()); err == nil {
		t.Error("Snapshot succeeded although the aggregation failed")
	}
	if _, err := svc.Report(context.Background(), 0); err == nil {
		t.Error("Report succeeded although the aggregation failed")
	}
}

func TestReportPriceMathAndAggregation(t *testing.T) {
	now := time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC)
	priced := &models.StoragePricing{Currency: "USD", StoragePerGiBMonth: 0.023, EgressPerGiB: 0.09}
	euro := &models.StoragePricing{Currency: "EUR", StoragePerGiBMonth: 0.01}
	repo := &fakeRepo{
		live: []Row{
			{TargetID: "a", Kind: KindBackup, JobID: "j1", Database: "d1", Objects: 2, Bytes: 10 * gib},
			{TargetID: "a", Kind: KindBackup, JobID: "j2", Database: "d1", Objects: 1, Bytes: 5 * gib},
			{TargetID: "b", Kind: KindCopy, JobID: "j1", Database: "d1", Objects: 2, Bytes: 10 * gib},
			{TargetID: "b", Kind: KindRetained, JobID: "j3", Database: "d2", Objects: 1, Bytes: 2 * gib},
			{TargetID: "b", Kind: KindPITR, StreamID: "pst", Objects: 9, Bytes: 4 * gib},
			{TargetID: "gone", Kind: KindCopy, JobID: "j1", Database: "d1", Objects: 1, Bytes: gib},
		},
		days: map[string][]Row{
			"2026-03-03": {{TargetID: "a", Kind: KindBackup, Bytes: 8 * gib}, {TargetID: "a", Kind: KindPITR, Bytes: gib}},
			"2026-03-05": {{TargetID: "a", Kind: KindBackup, Bytes: 15 * gib}},
			"2026-02-01": {{TargetID: "a", Kind: KindBackup, Bytes: 99 * gib}},
		},
		reads: []Read{
			{TargetID: "a", Source: EgressVerification, Bytes: gib, At: now.Add(-time.Hour)},
			{TargetID: "a", Source: EgressCopy, Bytes: 2 * gib, At: now.AddDate(0, 0, -3)},
			{TargetID: "a", Source: EgressRestoreTest, Bytes: 100 * gib, At: now.AddDate(0, 0, -40)}, // outside the window
		},
	}
	svc := New(repo, fakeTargets{target("a", "Alpha", true, priced), target("b", "Beta", false, euro), target("c", "Cold", false, nil)},
		WithClock(func() time.Time { return now }))
	rep, err := svc.Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if rep.From != "2026-02-27" || rep.To != "2026-03-05" || rep.Days != 7 || rep.Basis == "" {
		t.Errorf("range = %s..%s (%d days), basis %q", rep.From, rep.To, rep.Days, rep.Basis)
	}
	by := map[string]TargetUsage{}
	for _, u := range rep.Targets {
		by[u.TargetID] = u
	}
	a := by["a"]
	if a.Name != "Alpha" || a.StoredBytes != 15*gib || a.Objects != 3 || a.ByKind[KindBackup] != 15*gib {
		t.Errorf("target a = %+v", a)
	}
	if a.MonthlyCost == nil || *a.MonthlyCost != 0.345 { // 15 GiB x 0.023
		t.Errorf("a monthly cost = %v, want 0.345", a.MonthlyCost)
	}
	if a.Egress.Bytes != 3*gib || a.Egress.BySource[EgressVerification] != gib || a.Egress.BySource[EgressCopy] != 2*gib || a.Egress.BySource[EgressRestoreTest] != 0 {
		t.Errorf("a egress = %+v; want the 40-day-old read left out", a.Egress)
	}
	if a.Egress.Cost == nil || *a.Egress.Cost != 0.27 { // 3 GiB x 0.09
		t.Errorf("a egress cost = %v, want 0.27", a.Egress.Cost)
	}
	if len(a.History) != 2 || a.History[0] != (DayPoint{"2026-03-03", 9 * gib}) || a.History[1] != (DayPoint{"2026-03-05", 15 * gib}) || a.GrowthBytes != 6*gib {
		t.Errorf("a history = %+v, growth %d; want the January row outside the range left out", a.History, a.GrowthBytes)
	}

	b := by["b"]
	if b.StoredBytes != 16*gib || b.ByKind[KindCopy] != 10*gib || b.ByKind[KindRetained] != 2*gib || b.ByKind[KindPITR] != 4*gib {
		t.Errorf("target b = %+v; want copies, retained copies and PITR chunks counted", b)
	}
	if b.MonthlyCost == nil || *b.MonthlyCost != 0.16 || b.Egress.Cost == nil || *b.Egress.Cost != 0 {
		t.Errorf("b costs = %v / %v", b.MonthlyCost, b.Egress.Cost)
	}
	if c := by["c"]; c.StoredBytes != 0 || c.MonthlyCost != nil || c.Egress.Cost != nil || len(c.History) != 0 {
		t.Errorf("an unpriced empty target = %+v", c)
	}
	if g := by["gone"]; g.Name != "gone" || g.StoredBytes != gib {
		t.Errorf("a deleted target with data = %+v", g)
	}
	if len(rep.Totals) != 2 || rep.Totals[0] != (Currency{"EUR", 0.16, 0}) || rep.Totals[1] != (Currency{"USD", 0.345, 0.27}) {
		t.Errorf("totals = %+v; want one entry per currency", rep.Totals)
	}
	if len(rep.Jobs) != 3 || rep.Jobs[0].JobID != "j1" || rep.Jobs[0].Bytes != 21*gib || rep.Jobs[1].JobID != "j2" {
		t.Errorf("jobs = %+v; want j1 (primary + copies) first, no PITR", rep.Jobs)
	}
	if len(rep.Databases) != 2 || rep.Databases[0].Database != "d1" || rep.Databases[0].Bytes != 26*gib {
		t.Errorf("databases = %+v", rep.Databases)
	}
}

func TestReportRejectsARangeOutOfBounds(t *testing.T) {
	svc := New(&fakeRepo{}, fakeTargets{})
	for _, days := range []int{-1, MaxDays + 1} {
		if _, err := svc.Report(context.Background(), days); !errors.Is(err, ErrInvalid) {
			t.Errorf("Report(%d) = %v; want ErrInvalid", days, err)
		}
	}
	rep, err := svc.Report(context.Background(), 0)
	if err != nil || rep.Days != DefaultDays || rep.Targets == nil || rep.Totals == nil {
		t.Errorf("default report = %+v, %v", rep, err)
	}
}

func TestReportUsesTheLatestSnapshotOverTheLiveRecords(t *testing.T) {
	repo := &fakeRepo{live: []Row{{TargetID: "a", Kind: KindBackup, Objects: 1, Bytes: 7}}}
	svc := New(repo, fakeTargets{target("a", "A", true, nil)})
	if rep, _ := svc.Report(context.Background(), 1); rep.Targets[0].StoredBytes != 7 {
		t.Fatalf("before a snapshot the report reads the live records: %+v", rep.Targets[0])
	}
	if _, err := svc.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.live[0].Bytes = 70 // changes after the snapshot are not read per request
	if rep, _ := svc.Report(context.Background(), 1); rep.Targets[0].StoredBytes != 7 {
		t.Errorf("the report must reuse the snapshot: %+v", rep.Targets[0])
	}
}
