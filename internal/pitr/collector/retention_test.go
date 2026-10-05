package collector

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

func ts(t uint32) pitr.Timestamp { return pitr.Timestamp{T: t, I: 1} }

func base(id string, started time.Time, before, after uint32) *models.BackupRecord {
	return &models.BackupRecord{ID: id, Scope: models.ScopeInstance, PITRStreamID: "str_a", Status: models.StatusCompleted,
		StartedAt: started, TBefore: &pitr.OpTime{TS: ts(before)}, TAfter: &pitr.OpTime{TS: ts(after)}}
}

func chunkOf(id, chain string, from, to uint32, status pitr.ChunkStatus) *pitr.Chunk {
	return &pitr.Chunk{ID: id, ChainID: chain, From: ts(from), To: ts(to), Status: status}
}

func TestPlanRetention(t *testing.T) {
	now := time.Unix(100_000, 0).UTC()
	st := &pitr.Stream{ID: "str_a", BaseKeepCount: 2, OplogMaxDays: 0}
	ended := time.Unix(1, 0)
	chains := []*pitr.Chain{
		{ChainID: "old", Start: ts(10), End: ts(50), EndReason: pitr.EndGap, EndedAt: &ended},
		{ChainID: "cur", Start: ts(100)},
		{ChainID: "new_open"},
	}
	chunks := map[string][]*pitr.Chunk{
		"old": {chunkOf("o1", "old", 10, 30, pitr.ChunkCommitted), chunkOf("o2", "old", 30, 50, pitr.ChunkCommitted)},
		"cur": {
			chunkOf("c1", "cur", 100, 200, pitr.ChunkCommitted), chunkOf("c2", "cur", 200, 300, pitr.ChunkCommitted),
			chunkOf("c3", "cur", 300, 400, pitr.ChunkCommitted), chunkOf("c4", "cur", 400, 500, pitr.ChunkCommitted),
			chunkOf("c5", "cur", 500, 600, pitr.ChunkSuperseded),
		},
	}
	pinned := base("b_pinned", now.Add(-50*time.Hour), 110, 120)
	pinned.Pinned = true
	bases := []*models.BackupRecord{
		base("b_new", now.Add(-time.Hour), 410, 420),
		base("b_mid", now.Add(-10*time.Hour), 310, 320),
		base("b_old", now.Add(-20*time.Hour), 210, 220),
		pinned,
		base("b_gap", now.Add(-60*time.Hour), 15, 25), // covered by the ended chain
	}
	plan := PlanRetention(now, st, bases, chains, chunks)
	slices.Sort(plan.DeleteBases)
	if !slices.Equal(plan.DeleteBases, []string{"b_gap", "b_old"}) {
		t.Fatalf("bases deleted %v", plan.DeleteBases)
	}
	slices.Sort(plan.DeleteChunks)
	// The pinned base (T_before 110) keeps cur from c1 on; the ended chain loses its
	// only base and goes whole; the superseded chunk goes.
	if !slices.Equal(plan.DeleteChunks, []string{"c5", "o1", "o2"}) {
		t.Fatalf("chunks deleted %v", plan.DeleteChunks)
	}

	pinned.Pinned = false
	plan = PlanRetention(now, st, bases, chains, chunks)
	slices.Sort(plan.DeleteChunks)
	if !slices.Equal(plan.DeleteChunks, []string{"c1", "c2", "c5", "o1", "o2"}) {
		t.Fatalf("chunks deleted without the pin %v", plan.DeleteChunks)
	}

	// The newest eligible base is kept even beyond the count.
	st.BaseKeepCount = 0
	st.BaseKeepDays = 1
	plan = PlanRetention(now.Add(100*time.Hour), st, bases, chains, chunks)
	if slices.Contains(plan.DeleteBases, "b_new") || !slices.Contains(plan.DeleteBases, "b_mid") {
		t.Fatalf("bases deleted much later %v", plan.DeleteBases)
	}

	// oplog_max_days cuts chunks whatever the bases.
	st.BaseKeepCount, st.OplogMaxDays = 10, 1
	plan = PlanRetention(time.Unix(450+24*3600, 0), st, bases, chains, chunks)
	if !slices.Contains(plan.DeleteChunks, "c3") || slices.Contains(plan.DeleteChunks, "c4") {
		t.Fatalf("chunks deleted with oplog_max_days %v", plan.DeleteChunks)
	}
}

func TestRetentionDeletesWithGraceAndPurges(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	for range 3 {
		fx.f.add(t, 5)
		fx.step(w)
	}
	chain := fx.state().ChainID
	cs := fx.chunks(chain)
	if len(cs) != 4 {
		t.Fatalf("%d chunks", len(cs))
	}
	// One base taken during the last chunk: the chunks that end before its
	// T_before go, the one ending at it stays.
	b := base("bkp_b", fx.clock.Now(), 0, 0)
	b.TBefore.TS, b.TAfter.TS = cs[2].To, cs[3].To
	if err := fx.repo.SaveBackupRecord(ctx, b); err != nil {
		t.Fatal(err)
	}
	fx.svc.cfg.Bases = fx.repo.ListBaseBackups
	fx.svc.cfg.UpdateBase = fx.repo.UpdateBackupRecord
	fx.svc.cfg.DeleteGrace = func() time.Duration { return 24 * time.Hour }
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	live, _ := fx.repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: fx.stream.ID, ChainID: chain, Live: true})
	if len(live) != 2 || live[0].ID != cs[2].ID || fx.objects() != 4 {
		t.Fatalf("live %d, objects %d after retention", len(live), fx.objects())
	}
	status, err := fx.svc.Status(ctx, fx.stream.ID)
	if err != nil || len(status.Windows) != 1 || status.Windows[0].Start != cs[3].To || status.Windows[0].End != cs[3].To {
		t.Fatalf("windows %+v, %v", status.Windows, err)
	}
	fx.clock.advance(25 * time.Hour)
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if fx.objects() != 2 {
		t.Fatalf("%d objects after the purge", fx.objects())
	}
	all := fx.chunks(chain)
	if all[0].Status != pitr.ChunkPruned || all[1].Status != pitr.ChunkPruned || all[2].Status != pitr.ChunkCommitted {
		t.Fatalf("statuses %s %s %s", all[0].Status, all[1].Status, all[2].Status)
	}
}

func TestVerifyChunks(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.svc.cfg.Decryptor = func() *encryption.Decryptor { return fx.dec }
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	fx.f.add(t, 5)
	fx.step(w)
	res, err := fx.svc.VerifyChunks(ctx)
	if err != nil || res.Verified != 2 || res.Failed != 0 || res.Breaks != 0 {
		t.Fatalf("verify = %+v, %v", res, err)
	}
	if again, _ := fx.svc.VerifyChunks(ctx); again.Verified != 0 {
		t.Fatalf("verified again at once: %+v", again)
	}
	cs := fx.chunks(fx.state().ChainID)
	if _, err := fx.storage.Save(ctx, cs[1].StorageKey, bytes.NewReader([]byte("tampered"))); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.VerifyChunk(ctx, cs[1]); !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("tampered chunk: %v", err)
	}
	fx.clock.advance(ReverifyAfter + time.Hour)
	res, err = fx.svc.VerifyChunks(ctx)
	if err != nil || res.Verified != 2 || res.Failed != 1 {
		t.Fatalf("verify after tampering = %+v, %v", res, err)
	}
	if cs = fx.chunks(fx.state().ChainID); cs[1].VerifyError == "" || cs[0].VerifyError != "" {
		t.Fatalf("verify errors %q %q", cs[0].VerifyError, cs[1].VerifyError)
	}
}

func TestDeleteStreamWaitsForThePurge(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	w := fx.worker()
	fx.step(w)
	fx.step(w)
	if err := fx.svc.DeleteStream(ctx, fx.stream.ID); !errors.Is(err, ErrStillEnabled) {
		t.Fatalf("delete enabled: %v", err)
	}
	st, _ := fx.repo.GetStream(ctx, fx.stream.ID)
	st.Enabled = false
	if err := fx.repo.UpdateStream(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.DeleteStream(ctx, fx.stream.ID); !errors.Is(err, ErrChunksPending) {
		t.Fatalf("delete with chunks: %v", err)
	}
	fx.clock.advance(models.GraceDuration(models.DefaultDeleteGraceDays) + time.Hour)
	if err := fx.svc.RetainStream(ctx, fx.stream.ID); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.DeleteStream(ctx, fx.stream.ID); err != nil {
		t.Fatalf("delete after the purge: %v", err)
	}
	if fx.objects() != 0 {
		t.Fatalf("%d objects left", fx.objects())
	}
}
