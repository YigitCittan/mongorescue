package collector

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// TestRetentionSkipsWhatARestorePinned is the soak failure in miniature: a
// restore plans base B1 and its chunks, a newer base makes B1 fall out of the
// base count, and retention (no grace period) runs twice while the restore still
// reads. The pinned base and chunks survive, objects included, and go on the
// first runs after the pin is released.
func TestRetentionSkipsWhatARestorePinned(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	pins := pitr.NewPinSet(0, fx.clock.Now)
	fx.svc.cfg.Pins = pins
	var logs bytes.Buffer
	fx.svc.logger = slog.New(slog.NewTextHandler(&logs, nil))
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	for range 5 {
		fx.f.add(t, 5)
		fx.step(w)
	}
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	if len(cs) < 6 {
		t.Fatalf("%d chunks", len(cs))
	}
	st := *fx.stream
	st.BaseKeepCount, st.BaseKeepDays = 1, 0
	if err := fx.repo.UpdateStream(ctx, &st); err != nil {
		t.Fatal(err)
	}
	b1 := base("bkp_1", fx.clock.Now().Add(-2*time.Hour), 0, 0)
	b1.TBefore.TS, b1.TAfter.TS = cs[0].To, cs[1].To
	b2 := base("bkp_2", fx.clock.Now().Add(-time.Hour), 0, 0)
	b2.TBefore.TS, b2.TAfter.TS = cs[3].To, cs[4].To
	for _, b := range []*models.BackupRecord{b1, b2} {
		if err := fx.repo.SaveBackupRecord(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	fx.svc.cfg.UpdateBase = fx.repo.UpdateBackupRecord
	fx.svc.cfg.DeleteGrace = func() time.Duration { return 0 }

	// The restore of B1 to the end of cs[3] plans and pins cs[0..3].
	limit := cs[3].To
	plan, err := pitr.PlanRestore(ctx, &st, pitr.PlanSource{Repo: fx.repo, HasKey: func(string) bool { return true },
		Bases: []pitr.Base{{ID: b1.ID, TBefore: *b1.TBefore, TAfter: *b1.TAfter}}}, pitr.Target{TS: &limit})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(plan.Chunks))
	for i, c := range plan.Chunks {
		ids[i] = c.ID
	}
	release := pins.Pin("restore of B1", []string{b1.ID}, ids)
	if err = pitr.CheckPlanLive(ctx, fx.repo, plan); err != nil {
		t.Fatal(err)
	}
	// A deletion that raced the pin (soft-deleted before retention saw it) is not
	// purged either.
	if _, err = fx.repo.DeleteChunks(ctx, []string{cs[1].ID}, fx.clock.Now(), fx.clock.Now()); err != nil {
		t.Fatal(err)
	}

	retain := func() {
		t.Helper()
		fx.clock.advance(time.Minute)
		if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
			t.Fatal(err)
		}
	}
	retain()
	retain()
	if rec, err := fx.repo.GetBackupRecord(ctx, b1.ID); err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("pinned base %+v, %v", rec, err)
	}
	for _, c := range plan.Chunks {
		if _, err := fx.storage.Stat(ctx, c.StorageKey); err != nil {
			t.Fatalf("object of pinned chunk %s: %v", c.ID, err)
		}
	}
	for _, c := range fx.chunks(chain) {
		if c.Status == pitr.ChunkPruned {
			t.Fatalf("chunk %s was pruned during the restore", c.ID)
		}
	}
	if !strings.Contains(logs.String(), "skipped objects pinned") || !strings.Contains(logs.String(), "pinned_purges=1") {
		t.Fatalf("no log of the skipped objects:\n%s", logs.String())
	}

	// Once the restore ends, the next runs delete B1 and purge its chunks.
	release()
	retain()
	retain()
	if rec, err := fx.repo.GetBackupRecord(ctx, b1.ID); err != nil || !rec.Status.Deleted() {
		t.Fatalf("base after the release %+v, %v", rec, err)
	}
	all := fx.chunks(chain)
	for _, c := range all[:3] {
		if c.Status != pitr.ChunkPruned {
			t.Fatalf("chunk %s is %s after the release", c.ID, c.Status)
		}
		if _, err := fx.storage.Stat(ctx, c.StorageKey); err == nil {
			t.Fatalf("object of chunk %s left after the release", c.ID)
		}
	}
	if all[3].Status != pitr.ChunkCommitted || all[3].DeletedAt != nil {
		t.Fatalf("chunk %s at B2's T_before is %s", all[3].ID, all[3].Status)
	}
}

// TestRetentionIgnoresAStalePin checks that a pin never released stops counting
// after its maximum age, with a warning.
func TestRetentionIgnoresAStalePin(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	pins := pitr.NewPinSet(time.Hour, fx.clock.Now)
	fx.svc.cfg.Pins = pins
	var logs bytes.Buffer
	fx.svc.logger = slog.New(slog.NewTextHandler(&logs, nil))
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 5)
	fx.step(w)
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	pins.Pin("leaked", nil, []string{cs[0].ID})
	if _, err := fx.repo.DeleteChunks(ctx, []string{cs[0].ID}, fx.clock.Now(), fx.clock.Now()); err != nil {
		t.Fatal(err)
	}
	fx.clock.advance(time.Minute)
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if fx.chunks(chain)[0].Status == pitr.ChunkPruned {
		t.Fatal("a young pin did not hold the purge")
	}
	fx.clock.advance(2 * time.Hour)
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if fx.chunks(chain)[0].Status != pitr.ChunkPruned || pins.Len() != 0 {
		t.Fatalf("a stale pin still holds the purge (%d pins)", pins.Len())
	}
	if !strings.Contains(logs.String(), "older than its maximum age") {
		t.Fatalf("no warning about the stale pin:\n%s", logs.String())
	}
}
