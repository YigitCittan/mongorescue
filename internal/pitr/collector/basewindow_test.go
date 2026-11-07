package collector

import (
	"context"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// ratioObserver records the base window ratio; the other Observer methods are
// never called by observeBaseWindow.
type ratioObserver struct {
	Observer
	ratio map[string]float64
}

func (o *ratioObserver) SetPITRBaseWindowRatio(stream string, ratio float64, ok bool) {
	if !ok {
		delete(o.ratio, stream)
		return
	}
	o.ratio[stream] = ratio
}

// windowOf returns an oplog window spanning secs seconds.
func windowOf(secs uint32) pitr.OplogWindow {
	return pitr.OplogWindow{Oldest: pitr.Timestamp{T: 1000}, Newest: pitr.Timestamp{T: 1000 + secs}}
}

func TestBaseWindowRiskIsRaisedOncePerEpisode(t *testing.T) {
	fx := newFixture(t)
	obs := &ratioObserver{ratio: map[string]float64{}}
	fx.svc.cfg.Observer = obs
	// The last successful base took 50 minutes; a failed one and an older, longer
	// one do not count as the last.
	fx.svc.cfg.Bases = func(context.Context, string) ([]*models.BackupRecord, error) {
		return []*models.BackupRecord{
			{ID: "failed", Status: models.StatusFailed, DurationSeconds: 99999},
			{ID: "last", Status: models.StatusCompleted, DurationSeconds: 3000},
			{ID: "older", Status: models.StatusCompleted, DurationSeconds: 4000},
		}, nil
	}
	w := fx.worker()
	ctx := context.Background()
	now := fx.svc.now()
	observe := func(window uint32) {
		t.Helper()
		now = now.Add(2 * time.Minute)
		w.observeBaseWindow(ctx, windowOf(window), now)
	}
	risk := func() int { return fx.events.count(events.PITRBaseWindowRisk) }

	// A window of 10000 s: 3000 / 10000 = 0.3, below 0.5.
	observe(10000)
	if risk() != 0 || obs.ratio[fx.stream.ID] != 0.3 {
		t.Fatalf("events %d, ratio %v", risk(), obs.ratio)
	}
	// The oplog shrinks to 5000 s: 0.6 raises the event, once however long it lasts.
	for range 3 {
		observe(5000)
	}
	if risk() != 1 || obs.ratio[fx.stream.ID] != 0.6 {
		t.Fatalf("events %d, ratio %v", risk(), obs.ratio)
	}
	if bw := w.snapshot().BaseWindow; bw == nil || !bw.AtRisk || bw.ExpectedBaseSeconds != 4000 || bw.OplogWindowSeconds != 5000 {
		t.Fatalf("live base window %+v", bw)
	}
	// An empty window knows no ratio and neither ends nor raises the episode.
	observe(0)
	if _, ok := obs.ratio[fx.stream.ID]; ok || risk() != 1 {
		t.Fatalf("events %d, ratio %v", risk(), obs.ratio)
	}
	observe(5000)
	if risk() != 1 {
		t.Fatalf("%d events after the empty window", risk())
	}
	// Exactly at the threshold is not above it: the episode ends and re-arms.
	observe(6000)
	observe(5000)
	if risk() != 2 {
		t.Fatalf("%d events after the episode ended and began again", risk())
	}
	// A restarted collector raises it again while the risk lasts.
	restarted := fx.worker()
	restarted.observeBaseWindow(ctx, windowOf(5000), now)
	if risk() != 3 {
		t.Fatalf("%d events after a restart", risk())
	}
}

func TestBaseWindowRiskFollowsTheSetting(t *testing.T) {
	fx := newFixture(t)
	ratio := 0.9
	fx.svc.cfg.BaseWindowRiskRatio = func() float64 { return ratio }
	fx.svc.cfg.Bases = func(context.Context, string) ([]*models.BackupRecord, error) {
		return []*models.BackupRecord{{ID: "b", Status: models.StatusCompleted, DurationSeconds: 3000}}, nil
	}
	w := fx.worker()
	now := fx.svc.now()
	w.observeBaseWindow(context.Background(), windowOf(5000), now) // 0.6 is below 0.9
	if n := fx.events.count(events.PITRBaseWindowRisk); n != 0 {
		t.Fatalf("%d events below the configured ratio", n)
	}
	ratio = 0.5
	w.observeBaseWindow(context.Background(), windowOf(5000), now.Add(time.Second))
	if n := fx.events.count(events.PITRBaseWindowRisk); n != 1 {
		t.Fatalf("%d events above the configured ratio", n)
	}
}

func TestBaseWindowWithoutABase(t *testing.T) {
	fx := newFixture(t)
	fx.svc.cfg.Bases = func(context.Context, string) ([]*models.BackupRecord, error) { return nil, nil }
	w := fx.worker()
	w.observeBaseWindow(context.Background(), windowOf(100), fx.svc.now())
	if w.snapshot().BaseWindow != nil || fx.events.count(events.PITRBaseWindowRisk) != 0 {
		t.Fatal("a stream without a successful base is at risk")
	}
}
