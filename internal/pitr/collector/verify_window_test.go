package collector

import (
	"context"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func TestMissingChunkSplitsTheWindow(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.svc.cfg.Decryptor = func() *encryption.Decryptor { return fx.dec }
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	for range 4 {
		fx.f.add(t, 3)
		fx.step(w)
	}
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	if len(cs) != 5 {
		t.Fatalf("%d chunks", len(cs))
	}
	// A base taken during the second chunk.
	b := &models.BackupRecord{ID: "bkp_base", Scope: models.ScopeInstance, PITRStreamID: fx.stream.ID, Status: models.StatusCompleted,
		StartedAt: fx.clock.Now(), TBefore: &pitr.OpTime{TS: cs[1].From, Term: 1}, TAfter: &pitr.OpTime{TS: cs[1].To, Term: 1}}
	if err := fx.repo.SaveBackupRecord(ctx, b); err != nil {
		t.Fatal(err)
	}
	status, err := fx.svc.Status(ctx, fx.stream.ID)
	if err != nil || len(status.Windows) != 1 || !status.Windows[0].Open || status.Windows[0].End != cs[4].To {
		t.Fatalf("windows before the loss %+v, %v", status.Windows, err)
	}

	// The object of the fourth chunk disappears from storage; the sweep finds it.
	if err = fx.storage.Delete(ctx, cs[3].StorageKey); err != nil {
		t.Fatal(err)
	}
	res, err := fx.svc.VerifyChunks(ctx)
	if err != nil || res.Failed != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	status, err = fx.svc.Status(ctx, fx.stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Windows) != 1 || status.Windows[0].Open || status.Windows[0].End != cs[2].To {
		t.Fatalf("windows after the loss %+v: the window must end before the missing chunk and no longer grow", status.Windows)
	}
	if c := status.Chains[0]; c.Corrupt != 1 || len(c.Segments) != 2 {
		t.Fatalf("chain %+v", c)
	}

	// A base after the hole opens a window again; one spanning the hole is not
	// eligible.
	across := &models.BackupRecord{ID: "bkp_across", Scope: models.ScopeInstance, PITRStreamID: fx.stream.ID, Status: models.StatusCompleted,
		StartedAt: fx.clock.Now(), TBefore: &pitr.OpTime{TS: cs[2].From, Term: 1}, TAfter: &pitr.OpTime{TS: cs[4].To, Term: 1}}
	if err = fx.repo.SaveBackupRecord(ctx, across); err != nil {
		t.Fatal(err)
	}
	if status, err = fx.svc.Status(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	for _, bs := range status.Bases {
		if bs.ID == "bkp_across" && bs.Eligible {
			t.Fatal("a base whose [T_before, T_after] crosses the missing chunk is eligible")
		}
	}
}
